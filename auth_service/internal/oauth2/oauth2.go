// Package oauth2 实现 OAuth2.0 授权码模式客户端（Authorization Code + PKCE）。
//
// 定位说明（对应 ADR-004）：
//
//	本平台是 OAuth2.0 **客户端**（Relying Party），不是 Provider。
//	我们对接企业既有 IdP（Keycloak / Authing / 企业微信等）完成 SSO，
//	然后用 IdP 返回的用户身份自签本系统 JWT。
//	不建设 /.well-known/openid-configuration、动态客户端注册等 Provider 职责。
//
// 本文件只做协议动作：拼授权链接、换 token、拉 userinfo、算 PKCE。
// 身份 → 本地用户的映射与 JWT 签发在 service 层。
package oauth2

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/logger"
)

// Client OAuth2.0 客户端
type Client struct {
	http *http.Client
}

// NewClient 构造客户端。timeout 建议取 IdP 的响应超时。
func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{http: &http.Client{Timeout: timeout}}
}

// AuthorizeURL 生成授权跳转地址。
//
// 参数：
//   - state 由调用方生成的随机串，用于回调时防 CSRF
//   - codeChallenge PKCE 的 S256 挑战值（verifier 的 base64url(SHA256)）
func (c *Client) AuthorizeURL(idp conf.IdPConf, state, codeChallenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", idp.ClientID)
	if idp.RedirectURI != "" {
		q.Set("redirect_uri", idp.RedirectURI)
	}
	scopes := idp.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)

	// PKCE：IdP 支持时才带，避免老旧 IdP 因未知参数报错
	if idp.UsePKCE && codeChallenge != "" {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", "S256")
	}

	sep := "?"
	if strings.Contains(idp.AuthorizeURL, "?") {
		sep = "&"
	}
	return idp.AuthorizeURL + sep + q.Encode()
}

// TokenResult IdP 返回的令牌
type TokenResult struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	IDToken      string `json:"id_token"`
	Scope        string `json:"scope"`
}

// Exchange 用授权码换取 IdP 令牌。
//
// codeVerifier 为空表示该 IdP 未启用 PKCE。
func (c *Client) Exchange(ctx context.Context, idp conf.IdPConf, code, codeVerifier, redirectURI string) (*TokenResult, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", idp.ClientID)
	if idp.ClientSecret != "" {
		form.Set("client_secret", idp.ClientSecret)
	}
	// redirect_uri 必须与授权请求时完全一致，否则 IdP 拒绝
	if redirectURI == "" {
		redirectURI = idp.RedirectURI
	}
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, idp.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errno.Wrap(errno.ErrOAuthExchangeFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		logger.Warn(ctx, "oauth2 token exchange request failed", "idp", idp.Name, "err", err)
		return nil, errno.Wrap(errno.ErrIdPUnreachable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errno.Wrap(errno.ErrOAuthExchangeFailed, err)
	}
	if resp.StatusCode != http.StatusOK {
		// 不把 IdP 原文回给前端：可能含 client_secret 等敏感信息
		logger.Warn(ctx, "oauth2 token exchange rejected",
			"idp", idp.Name, "status", resp.StatusCode, "body", truncate(string(body), 300))
		return nil, errno.Wrap(errno.ErrOAuthExchangeFailed,
			fmt.Errorf("idp 返回状态 %d", resp.StatusCode))
	}

	var tr TokenResult
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, errno.Wrap(errno.ErrOAuthExchangeFailed, err)
	}
	if tr.AccessToken == "" && tr.IDToken == "" {
		return nil, errno.New(errno.ErrOAuthExchangeFailed)
	}
	return &tr, nil
}

// UserInfo 归一化后的用户信息
type UserInfo struct {
	Subject  string `json:"sub"`
	Username string `json:"preferred_username"`
	Nickname string `json:"name"`
	Email    string `json:"email"`
	Phone    string `json:"phone_number"`
	Avatar   string `json:"picture"`
	Roles    []string
	Raw      map[string]any
}

// FetchUserInfo 调用 userinfo 接口获取用户资料。
func (c *Client) FetchUserInfo(ctx context.Context, idp conf.IdPConf, accessToken string) (*UserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, idp.UserInfoURL, nil)
	if err != nil {
		return nil, errno.Wrap(errno.ErrOAuthExchangeFailed, err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errno.Wrap(errno.ErrIdPUnreachable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errno.Wrap(errno.ErrOAuthExchangeFailed, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errno.Wrap(errno.ErrOAuthExchangeFailed,
			fmt.Errorf("userinfo 返回状态 %d", resp.StatusCode))
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errno.Wrap(errno.ErrOAuthExchangeFailed, err)
	}

	info := &UserInfo{
		Subject:  str(raw, "sub"),
		Username: str(raw, "preferred_username"),
		Nickname: str(raw, "name"),
		Email:    str(raw, "email"),
		Phone:    str(raw, "phone_number"),
		Avatar:   str(raw, "picture"),
		Raw:      raw,
	}
	// roles 常见两种形态：数组或空格分隔字符串
	switch v := raw["roles"].(type) {
	case []any:
		for _, r := range v {
			if s, ok := r.(string); ok && s != "" {
				info.Roles = append(info.Roles, s)
			}
		}
	case string:
		info.Roles = strings.Fields(v)
	}
	return info, nil
}

// PKCE 相关

// NewVerifier 生成 PKCE code_verifier。
//
// RFC 7636 要求 43-128 字符，取 64 字节 base64url（86 字符）留足余量。
func NewVerifier() string {
	return randomURLSafe(64)
}

// CodeChallenge 由 verifier 计算 S256 挑战值
func CodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// NewState 生成防 CSRF 的随机 state
func NewState() string { return randomURLSafe(24) }

// randomURLSafe 生成 URL 安全随机串
func randomURLSafe(n int) string {
	b := make([]byte, n)
	if _, err := readRandom(b); err != nil {
		// 退化路径不应出现；用时间派生仅为避免 panic
		for i := range b {
			b[i] = byte(time.Now().UnixNano() >> (i % 8 * 8))
		}
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// UsernameFrom 按配置字段选择用户名，缺省回退链。
//
// 不同 IdP 字段名差异很大（preferred_username / login / sub / email），
// 因此允许配置显式指定，并提供合理回退。
func UsernameFrom(idp conf.IdPConf, info *UserInfo) string {
	if idp.UsernameField != "" {
		if v := str(info.Raw, idp.UsernameField); v != "" {
			return v
		}
	}
	for _, cand := range []string{info.Username, info.Subject, info.Email} {
		if cand != "" {
			return cand
		}
	}
	return ""
}

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
