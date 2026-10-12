package oauth2

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/errno"
)

// testIdp 构造测试 IdP 配置。
func testIdp() conf.IdPConf {
	return conf.IdPConf{
		Name:         "keycloak",
		ClientID:     "aegis",
		ClientSecret: "s3cret",
		AuthorizeURL: "https://idp.example.com/auth",
		TokenURL:     "https://idp.example.com/token",
		RedirectURI:  "https://app.example.com/cb",
		Scopes:       []string{"openid", "profile"},
	}
}

// TestAuthorizeURL 授权链接参数组装。
func TestAuthorizeURL(t *testing.T) {
	c := NewClient(0)
	idp := testIdp()
	u := c.AuthorizeURL(idp, "state-1", "challenge-1")

	if !strings.Contains(u, "response_type=code") {
		t.Errorf("缺少 response_type: %s", u)
	}
	if !strings.Contains(u, "client_id=aegis") {
		t.Errorf("缺少 client_id: %s", u)
	}
	if !strings.Contains(u, "redirect_uri=https%3A%2F%2Fapp.example.com%2Fcb") {
		t.Errorf("缺少 redirect_uri: %s", u)
	}
	if !strings.Contains(u, "state=state-1") {
		t.Errorf("缺少 state: %s", u)
	}
	if !strings.Contains(u, "scope=openid+profile") {
		t.Errorf("缺少 scope: %s", u)
	}

	// 未启用 PKCE：不带 code_challenge
	if strings.Contains(u, "code_challenge") {
		t.Errorf("未启用 PKCE 不应带 code_challenge: %s", u)
	}

	// 启用 PKCE：带 S256
	pkce := testIdp()
	pkce.UsePKCE = true
	u2 := c.AuthorizeURL(pkce, "s", "cc")
	if !strings.Contains(u2, "code_challenge=cc") || !strings.Contains(u2, "code_challenge_method=S256") {
		t.Errorf("PKCE 参数缺失: %s", u2)
	}

	// 无 scopes 时回落默认
	empty := testIdp()
	empty.Scopes = nil
	u3 := c.AuthorizeURL(empty, "s", "")
	if !strings.Contains(u3, "openid+profile+email") {
		t.Errorf("默认 scope 缺失: %s", u3)
	}

	// 授权 URL 已含 query：用 & 连接
	withQuery := testIdp()
	withQuery.AuthorizeURL = "https://idp.example.com/auth?tenant=x"
	u4 := c.AuthorizeURL(withQuery, "s", "")
	if !strings.HasPrefix(u4, "https://idp.example.com/auth?tenant=x&") {
		t.Errorf("query 拼接错误: %s", u4)
	}
}

// TestExchangeSuccess 正常换 token：表单字段与响应解析。
func TestExchangeSuccess(t *testing.T) {
	var gotForm urlValues
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotForm = urlValues{Values: r.Form}
		gotAuth = r.Header.Get("Content-Type")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-1", "token_type": "Bearer", "expires_in": 3600,
			"refresh_token": "rt-1", "id_token": "id-1",
		})
	}))
	t.Cleanup(srv.Close)

	idp := testIdp()
	idp.TokenURL = srv.URL
	c := NewClient(3 * time.Second)

	tr, err := c.Exchange(context.Background(), idp, "code-1", "verifier-1", "https://app.example.com/cb")
	if err != nil {
		t.Fatalf("Exchange 失败: %v", err)
	}
	if tr.AccessToken != "at-1" || tr.IDToken != "id-1" || tr.RefreshToken != "rt-1" {
		t.Errorf("令牌解析不符: %+v", tr)
	}
	// 表单字段
	for _, want := range []string{"grant_type=authorization_code", "code=code-1", "client_id=aegis", "client_secret=s3cret", "code_verifier=verifier-1"} {
		if !strings.Contains(gotForm.Encode(), want) {
			t.Errorf("表单缺少 %s: %s", want, gotForm.Encode())
		}
	}
	if !strings.HasPrefix(gotAuth, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type 不符: %s", gotAuth)
	}
}

// TestExchangeRejected IdP 返回非 200 → ErrOAuthExchangeFailed（不泄露原文）。
func TestExchangeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"含有 client_secret 的敏感信息"}`))
	}))
	t.Cleanup(srv.Close)

	idp := testIdp()
	idp.TokenURL = srv.URL
	_, err := NewClient(3*time.Second).Exchange(context.Background(), idp, "bad", "", "")
	if errno.CodeOf(err) != errno.ErrOAuthExchangeFailed {
		t.Fatalf("应 ErrOAuthExchangeFailed, 实际 %v", err)
	}
}

// TestExchangeUnreachable IdP 不可达 → ErrIdPUnreachable。
func TestExchangeUnreachable(t *testing.T) {
	idp := testIdp()
	idp.TokenURL = "http://127.0.0.1:1/token" // 必然连接失败
	_, err := NewClient(time.Second).Exchange(context.Background(), idp, "c", "", "")
	if errno.CodeOf(err) != errno.ErrIdPUnreachable {
		t.Fatalf("应 ErrIdPUnreachable, 实际 %v", err)
	}
}

// TestExchangeEmptyTokens 响应无任何令牌 → 失败。
func TestExchangeEmptyTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	idp := testIdp()
	idp.TokenURL = srv.URL

	_, err := NewClient(3*time.Second).Exchange(context.Background(), idp, "c", "", "")
	if errno.CodeOf(err) != errno.ErrOAuthExchangeFailed {
		t.Fatalf("应 ErrOAuthExchangeFailed, 实际 %v", err)
	}
}

// TestFetchUserInfo 解析 userinfo 字段与 roles 两种形态。
func TestFetchUserInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at-1" {
			t.Errorf("Authorization 头不符: %s", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"sub": "u-100", "preferred_username": "alice", "name": "Alice",
			"email": "a@x.io", "picture": "pic-url",
			"roles": []any{"user", "admin"},
		})
	}))
	t.Cleanup(srv.Close)

	idp := testIdp()
	idp.UserInfoURL = srv.URL
	info, err := NewClient(3*time.Second).FetchUserInfo(context.Background(), idp, "at-1")
	if err != nil {
		t.Fatalf("FetchUserInfo 失败: %v", err)
	}
	if info.Subject != "u-100" || info.Email != "a@x.io" {
		t.Errorf("字段解析不符: %+v", info)
	}
	if len(info.Roles) != 2 || info.Roles[0] != "user" || info.Roles[1] != "admin" {
		t.Errorf("roles 数组解析不符: %v", info.Roles)
	}

	// roles 为空格分隔字符串
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"sub": "u", "roles": "user admin"})
	}))
	t.Cleanup(srv2.Close)
	idp2 := testIdp()
	idp2.UserInfoURL = srv2.URL
	info2, _ := NewClient(3*time.Second).FetchUserInfo(context.Background(), idp2, "at")
	if len(info2.Roles) != 2 {
		t.Errorf("roles 字符串解析不符: %v", info2.Roles)
	}
}

// TestFetchUserInfoNon200 userinfo 非 200 → 失败。
func TestFetchUserInfoNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	idp := testIdp()
	idp.UserInfoURL = srv.URL

	_, err := NewClient(3*time.Second).FetchUserInfo(context.Background(), idp, "bad")
	if errno.CodeOf(err) != errno.ErrOAuthExchangeFailed {
		t.Fatalf("应 ErrOAuthExchangeFailed, 实际 %v", err)
	}
}

// TestCodeChallengeRFC7636 S256 挑战值符合 RFC 7636（base64url 无 padding 的 SHA256）。
func TestCodeChallengeRFC7636(t *testing.T) {
	verifier := NewVerifier()
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Errorf("verifier 长度应 43-128, 实际 %d", len(verifier))
	}
	challenge := CodeChallenge(verifier)
	if strings.ContainsAny(challenge, "=+/") {
		t.Errorf("挑战值应为 base64url 无 padding: %s", challenge)
	}
	// 确定性
	if CodeChallenge(verifier) != challenge {
		t.Error("挑战值应确定")
	}
}

// TestRandomLength state / verifier 长度与唯一性。
func TestRandomLength(t *testing.T) {
	states := map[string]bool{}
	for i := 0; i < 100; i++ {
		s := NewState()
		if len(s) != 32 { // 24 字节 base64url = 32 字符
			t.Fatalf("state 长度不符: %d", len(s))
		}
		if states[s] {
			t.Fatal("state 不应重复")
		}
		states[s] = true
	}
}

// TestUsernameFrom 用户名选择：显式字段 > 回退链。
func TestUsernameFrom(t *testing.T) {
	idp := testIdp()
	idp.UsernameField = "login"
	info := &UserInfo{
		Username: "preferred", Subject: "sub-1", Email: "e@x.io",
		Raw: map[string]any{"login": "custom-login"},
	}
	if got := UsernameFrom(idp, info); got != "custom-login" {
		t.Errorf("应优先使用配置字段: %q", got)
	}
	idp.UsernameField = ""
	if got := UsernameFrom(idp, info); got != "preferred" {
		t.Errorf("回退链应取 Username: %q", got)
	}
	empty := &UserInfo{Subject: "sub-only"}
	if got := UsernameFrom(idp, empty); got != "sub-only" {
		t.Errorf("应回退到 Subject: %q", got)
	}
	if got := UsernameFrom(idp, &UserInfo{}); got != "" {
		t.Errorf("全空应返回空: %q", got)
	}
}

// urlValues 轻量封装，便于断言表单。
type urlValues struct{ Values map[string][]string }

func (u urlValues) Encode() string {
	var parts []string
	for k, vs := range u.Values {
		for _, v := range vs {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "&")
}
