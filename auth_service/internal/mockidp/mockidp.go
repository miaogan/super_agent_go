// Package mockidp 实现一个内嵌的 OAuth2.0 / OIDC 身份提供方。
//
// 用途：**仅用于冒烟测试**。
//
// 为什么需要它：SSO 链路（授权跳转 → code → 换 token → userinfo → JWT）
// 涉及 4 次跨服务交互，若依赖真实 Keycloak，冒烟测试就无法在
// 单机 docker-compose 之外跑通。内嵌 IdP 让 OAuth2.0 全链路
// 在一个进程内闭环验证。
//
// 安全边界：
//   - 只在 mock.enabled=true 时启动
//   - 监听独立端口，不与任何生产端口冲突
//   - 用户池为固定演示账号，口令与 user_service 的演示账号一致
package mockidp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// User 演示用户
type User struct {
	Subject  string
	Username string
	Name     string
	Email    string
	Password string
	Roles    []string
}

// DefaultUser 冒烟测试默认账号
func DefaultUser() User {
	return User{
		Subject:  "keycloak-sub-1001",
		Username: "sso.user",
		Name:     "SSO 用户",
		Email:    "sso.user@aegis.dev",
		Password: "Aegis@2026",
		Roles:    []string{"user"},
	}
}

// Server 内嵌 IdP
type Server struct {
	mu sync.Mutex

	clientID     string
	clientSecret string
	redirectURI  string

	// code → 授权上下文
	codes map[string]*authCode

	// codeVerifier → challenge，PKCE 校验用
	verifiers map[string]string

	users map[string]User // username → user
}

// authCode 一次授权产生的 code
type authCode struct {
	username    string
	redirectURI string
	createdAt   time.Time
}

// New 构造内嵌 IdP
func New(clientID, clientSecret, redirectURI string) *Server {
	u := DefaultUser()
	return &Server{
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURI:  redirectURI,
		codes:        make(map[string]*authCode),
		verifiers:    make(map[string]string),
		users:        map[string]User{u.Username: u},
	}
}

// AddUser 追加演示用户
func (s *Server) AddUser(u User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.Username] = u
}

// Handler 返回 HTTP 路由
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// OIDC 发现文档 —— 让标准客户端能自动探测端点
	mux.HandleFunc("/.well-known/openid-configuration", s.handleDiscovery)

	// 授权端点。
	// 真实 IdP 这里会渲染登录页；内嵌版直接用 query 参数 username
	// 指定身份（等价于"自动登录了演示账号"），换取 code 后 302 回 redirect_uri。
	mux.HandleFunc("/authorize", s.handleAuthorize)

	// 令牌端点
	mux.HandleFunc("/token", s.handleToken)

	// 用户信息端点
	mux.HandleFunc("/userinfo", s.handleUserInfo)

	return mux
}

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	base := "http://" + r.Host
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/authorize",
		"token_endpoint":                        base + "/token",
		"userinfo_endpoint":                     base + "/userinfo",
		"jwks_uri":                              base + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
		"code_challenge_methods_supported":      []string{"S256"},
		"grant_types_supported":                 []string{"authorization_code"},
	})
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	state := q.Get("state")
	redirectURI := q.Get("redirect_uri")

	if clientID != s.clientID {
		http.Error(w, "invalid client_id", http.StatusBadRequest)
		return
	}
	if redirectURI == "" {
		redirectURI = s.redirectURI
	}
	// 内嵌 IdP 只允许回调到白名单地址，防开放重定向
	if redirectURI != s.redirectURI {
		http.Error(w, "redirect_uri not allowed", http.StatusBadRequest)
		return
	}
	if state == "" {
		http.Error(w, "state is required", http.StatusBadRequest)
		return
	}

	// 记住 PKCE challenge，token 端点校验
	if ch := q.Get("code_challenge"); ch != "" {
		s.mu.Lock()
		s.verifiers[ch] = q.Get("code_challenge_method")
		s.mu.Unlock()
	}

	// "登录"：优先取 username 参数，否则用唯一演示用户
	username := q.Get("username")
	if username == "" {
		username = DefaultUser().Username
	}

	s.mu.Lock()
	if _, ok := s.users[username]; !ok {
		s.mu.Unlock()
		http.Error(w, "unknown user", http.StatusBadRequest)
		return
	}
	code := fmt.Sprintf("mock-code-%d-%s", time.Now().UnixNano(), shortHash(username))
	s.codes[code] = &authCode{username: username, redirectURI: redirectURI, createdAt: time.Now()}
	s.mu.Unlock()

	// 302 跳回客户端，携带 code 与 state
	sep := "?"
	if strings.Contains(redirectURI, "?") {
		sep = "&"
	}
	target := fmt.Sprintf("%s%scode=%s&state=%s", redirectURI, sep, code, state)
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	if r.Form.Get("grant_type") != "authorization_code" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	if r.Form.Get("client_id") != s.clientID {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
		return
	}
	if s.clientSecret != "" && r.Form.Get("client_secret") != s.clientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
		return
	}
	// 回调 redirect_uri 必须与授权时一致
	if r.Form.Get("redirect_uri") != "" && r.Form.Get("redirect_uri") != s.redirectURI {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "redirect_uri_mismatch"})
		return
	}

	s.mu.Lock()
	ac, ok := s.codes[r.Form.Get("code")]
	if ok {
		delete(s.codes, r.Form.Get("code")) // 一次性
	}
	s.mu.Unlock()

	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	if time.Since(ac.createdAt) > 5*time.Minute {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "expired_token"})
		return
	}

	// PKCE 校验：若授权时带了 challenge，则 token 请求必须带匹配的 verifier
	if verifier := r.Form.Get("code_verifier"); verifier != "" {
		challenge := codeChallenge(verifier)
		s.mu.Lock()
		method, known := s.verifiers[challenge]
		delete(s.verifiers, challenge)
		s.mu.Unlock()
		if !known {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "PKCE 校验失败"})
			return
		}
		if method != "" && method != "S256" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "不支持的 code_challenge_method"})
			return
		}
	} else {
		// 反之：授权时带了 challenge 但 token 请求没带 verifier，同样拒绝
		s.mu.Lock()
		hasChallenge := len(s.verifiers) > 0
		s.mu.Unlock()
		if hasChallenge {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "缺少 code_verifier"})
			return
		}
	}

	s.mu.Lock()
	u := s.users[ac.username]
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  "mock-access-" + shortHash(u.Username),
		"refresh_token": "mock-refresh-" + shortHash(u.Username),
		"token_type":    "Bearer",
		"expires_in":    3600,
		"scope":         "openid profile email",
	})
}

func (s *Server) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_token"})
		return
	}

	username := ""
	for name, u := range s.users {
		if token == "mock-access-"+shortHash(name) {
			username = u.Username
			break
		}
	}
	if username == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_token"})
		return
	}

	s.mu.Lock()
	u := s.users[username]
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"sub":                u.Subject,
		"preferred_username": u.Username,
		"name":               u.Name,
		"email":              u.Email,
		"email_verified":     true,
		"roles":              u.Roles,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
