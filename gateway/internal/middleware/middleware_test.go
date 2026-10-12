package middleware

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	"github.com/aegis/pkg/identity"
)

// stubVerifier 桩校验器
type stubVerifier struct {
	id  *commonv1.Identity
	err error
	// gotToken 记录收到的令牌，断言提取逻辑
	gotToken string
	// gotScope 记录要求的 scope
	gotScope string
}

func (s *stubVerifier) Verify(_ context.Context, token, scope string) (*commonv1.Identity, error) {
	s.gotToken = token
	s.gotScope = scope
	if s.err != nil {
		return nil, s.err
	}
	return s.id, nil
}

func newCtx(method, uri string) *app.RequestContext {
	c := app.NewContext(0)
	c.Request.SetMethod(method)
	c.Request.SetRequestURI(uri)
	return c
}

// runChain 按 Hertz 真实的索引链语义跑中间件：
// 每个中间件调c.Next(ctx) 推进索引，
// 而不是"next 回调"——两者的短路行为完全不同。
func runChain(mws []app.HandlerFunc, final func(*app.RequestContext), c *app.RequestContext) {
	chain := append(app.HandlersChain{}, mws...)
	chain = append(chain, func(ctx context.Context, c *app.RequestContext) { final(c) })
	c.SetHandlers(chain)
	chain[0](context.Background(), c)
}

func TestExtractTokenFromBearerHeader(t *testing.T) {
	c := newCtx("GET", "/x")
	c.Request.Header.Set("Authorization", "Bearer tok-123")
	if got := ExtractToken(c); got != "tok-123" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractTokenCaseInsensitive(t *testing.T) {
	c := newCtx("GET", "/x")
	c.Request.Header.Set("Authorization", "bearer tok")
	if got := ExtractToken(c); got != "tok" {
		t.Fatalf("got %q", got)
	}
}

// ★ SSE 必须支持 query 取令牌：浏览器 EventSource API
//
//	无法设置请求头，只能挂在 URL 上。
func TestExtractTokenFromQuery(t *testing.T) {
	c := newCtx("GET", "/x?access_token=tok-sse")
	if got := ExtractToken(c); got != "tok-sse" {
		t.Fatalf("got %q", got)
	}
}

// 头优先于 query：同时存在时不能被 query 覆盖，
// 否则攻击者可以用 URL 里的令牌劫持带合法头的请求。
func TestExtractTokenHeaderWins(t *testing.T) {
	c := newCtx("GET", "/x?access_token=from-query")
	c.Request.Header.Set("Authorization", "Bearer from-header")
	if got := ExtractToken(c); got != "from-header" {
		t.Fatalf("got %q, 期望以 header 为准", got)
	}
}

// ★ 非 Bearer 头存在时必须返回空，不回退 query。
//
//	客户端显式声明了"我用的是别的认证方式"，
//	此时URL 里恰好带了个 access_token 就自动生效，
//	等于凭据注入。反过来 Bearer 头存在时才用 Bearer。
func TestExtractTokenNonBearerHeaderNoFallback(t *testing.T) {
	c := newCtx("GET", "/x?access_token=q")
	c.Request.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if got := ExtractToken(c); got != "" {
		t.Fatalf("非 Bearer 头不应回退到 query，实际 %q", got)
	}
}

func TestAuthAbortsWithoutToken(t *testing.T) {
	c := newCtx("GET", "/api/v1/agents")
	var reached bool
	runChain([]app.HandlerFunc{Auth(&stubVerifier{}, "")},
		func(*app.RequestContext) { reached = true }, c)

	if reached {
		t.Fatal("缺令牌时不应进入 handler")
	}
	if c.Response.StatusCode() != consts.StatusUnauthorized {
		t.Fatalf("status = %d, 期望 401", c.Response.StatusCode())
	}
}

func TestAuthInjectsIdentity(t *testing.T) {
	v := &stubVerifier{id: &commonv1.Identity{
		Uid: 1001, Username: "admin", Tenant: "acme", Scopes: []string{"admin"},
	}}
	c := newCtx("GET", "/api/v1/agents")
	c.Request.Header.Set("Authorization", "Bearer good")

	var gotUID int64
	var gotTenant string
	runChain([]app.HandlerFunc{Auth(v, "")}, func(c *app.RequestContext) {
		id := identity.From(ctxOfForTest(c))
		gotUID = id.GetUid()
		gotTenant = id.GetTenant()
	}, c)

	if gotUID != 1001 || gotTenant != "acme" {
		t.Fatalf("身份未透传: uid=%d tenant=%s", gotUID, gotTenant)
	}
	if v.gotToken != "good" {
		t.Fatalf("校验器收到的令牌 = %q", v.gotToken)
	}
}

// ★ 身份必须经由 pkg/identity 写入同一个 ctx key。
//
//	中间件自造 ctxKey 而 handler 用 identity.From 读取时，
//	身份被静默丢弃（uid=0），且不报任何错——
//	表现为"鉴权通过但所有下游都认为你是匿名"。
func TestAuthUsesSharedIdentityKey(t *testing.T) {
	v := &stubVerifier{id: &commonv1.Identity{Uid: 42, Tenant: "t"}}
	c := newCtx("GET", "/api/v1/agents")
	c.Request.Header.Set("Authorization", "Bearer t")

	var seen bool
	runChain([]app.HandlerFunc{Auth(v, "")}, func(c *app.RequestContext) {
		if identity.Uid(ctxOfForTest(c)) == 42 {
			seen = true
		}
	}, c)
	if !seen {
		t.Fatal("identity.From 读不到中间件写入的身份（ctx key 不一致）")
	}
}

func TestAuthHidesInternalError(t *testing.T) {
	// 下游返回含内部细节的错误，不能原样透给客户端
	internal := errors.New("dial tcp 10.0.0.5:8881: connect: connection refused")
	c := newCtx("GET", "/api/v1/agents")
	c.Request.Header.Set("Authorization", "Bearer t")

	runChain([]app.HandlerFunc{Auth(&stubVerifier{err: internal}, "")},
		func(*app.RequestContext) { t.Fatal("不应进入 handler") }, c)

	if c.Response.StatusCode() != consts.StatusUnauthorized {
		t.Fatalf("status = %d", c.Response.StatusCode())
	}
	body := string(c.Response.Body())
	if strings.Contains(body, "10.0.0.5") || strings.Contains(body, "connection refused") {
		t.Fatalf("内部错误细节泄露到响应: %s", body)
	}
}

func TestAuthPassesScope(t *testing.T) {
	v := &stubVerifier{id: &commonv1.Identity{Uid: 1}}
	c := newCtx("GET", "/x")
	c.Request.Header.Set("Authorization", "Bearer t")
	runChain([]app.HandlerFunc{Auth(v, "admin")}, func(*app.RequestContext) {}, c)
	if v.gotScope != "admin" {
		t.Fatalf("scope = %q", v.gotScope)
	}
}

func TestOptionalAuthAllowsAnonymous(t *testing.T) {
	c := newCtx("GET", "/api/v1/agents/public")
	var reached bool
	runChain([]app.HandlerFunc{OptionalAuth(&stubVerifier{})},
		func(*app.RequestContext) { reached = true }, c)
	if !reached {
		t.Fatal("OptionalAuth 应允许匿名通过")
	}
}

// 令牌无效时 OptionalAuth 应放行（匿名降级），而不是 401
func TestOptionalAuthIgnoresBadToken(t *testing.T) {
	c := newCtx("GET", "/x")
	c.Request.Header.Set("Authorization", "Bearer bad")
	var reached bool
	runChain([]app.HandlerFunc{OptionalAuth(&stubVerifier{err: errors.New("bad")})},
		func(*app.RequestContext) { reached = true }, c)
	if !reached {
		t.Fatal("令牌无效时应匿名放行")
	}
	if c.Response.StatusCode() == consts.StatusUnauthorized {
		t.Fatal("OptionalAuth 不应返回 401")
	}
}

func TestCORSReflectsOriginNotWildcard(t *testing.T) {
	// 配合 Allow-Credentials 时浏览器会拒绝 "*"
	c := newCtx("GET", "/x")
	c.Request.Header.Set("Origin", "https://app.example.com")

	runChain([]app.HandlerFunc{CORS([]string{"https://app.example.com"}, 0)},
		func(*app.RequestContext) {}, c)

	if got := string(c.Response.Header.Get("Access-Control-Allow-Origin")); got != "https://app.example.com" {
		t.Fatalf("Allow-Origin = %q", got)
	}
}

func TestCORSIgnoresUnknownOrigin(t *testing.T) {
	c := newCtx("GET", "/x")
	c.Request.Header.Set("Origin", "https://evil.com")
	runChain([]app.HandlerFunc{CORS([]string{"https://app.example.com"}, 0)},
		func(*app.RequestContext) {}, c)
	if got := c.Response.Header.Get("Access-Control-Allow-Origin"); len(got) != 0 {
		t.Fatalf("未授权 origin 不应被放行，实际 %q", got)
	}
}

func TestCORSHandlesPreflight(t *testing.T) {
	c := newCtx("OPTIONS", "/api/v1/agents")
	c.Request.Header.Set("Origin", "https://app.example.com")

	var reached bool
	runChain([]app.HandlerFunc{CORS([]string{"*"}, 0)},
		func(*app.RequestContext) { reached = true }, c)

	if reached {
		t.Fatal("预检请求不应下沉到 handler")
	}
	if c.Response.StatusCode() != consts.StatusNoContent {
		t.Fatalf("预检应返回 204，实际 %d", c.Response.StatusCode())
	}
}

// CORS 必须在鉴权之前：否则预检请求会被 401 拦掉，
// 浏览器直接判定跨域失败（表现为"接口不通"而非 CORS 错误）。
func TestCORSBeforeAuth(t *testing.T) {
	c := newCtx("OPTIONS", "/api/v1/agents")
	c.Request.Header.Set("Origin", "https://app.example.com")

	runChain([]app.HandlerFunc{
		CORS([]string{"*"}, 0),
		Auth(&stubVerifier{}, ""),
	}, func(*app.RequestContext) { t.Fatal("不应进入 handler") }, c)

	if c.Response.StatusCode() != consts.StatusNoContent {
		t.Fatalf("status = %d, 期望预检 204", c.Response.StatusCode())
	}
}

func TestTraceIDPropagates(t *testing.T) {
	c := newCtx("GET", "/x")
	c.Request.Header.Set("X-Request-Id", "trace-from-client")

	var got string
	runChain([]app.HandlerFunc{TraceID()}, func(c *app.RequestContext) {
		if v, ok := c.Get(TraceIDKey); ok {
			got, _ = v.(string)
		}
	}, c)

	if got != "trace-from-client" {
		t.Fatalf("客户端传入的 trace_id 应被沿用，实际 %q", got)
	}
	if h := string(c.Response.Header.Get("X-Request-Id")); h != "trace-from-client" {
		t.Fatalf("响应头未回传 trace id: %q", h)
	}
}

func TestTraceIDGeneratesWhenAbsent(t *testing.T) {
	c := newCtx("GET", "/x")
	var got string
	runChain([]app.HandlerFunc{TraceID()}, func(c *app.RequestContext) {
		if v, ok := c.Get(TraceIDKey); ok {
			got, _ = v.(string)
		}
	}, c)
	if len(got) != 24 { // 12 字节 hex
		t.Fatalf("自动生成的 trace id = %q", got)
	}
}

func TestTraceIDSetsCtx(t *testing.T) {
	// 免鉴权路由不经过 Auth，若TraceID 不落盘 ctx，
	// handler 的 ctxOf 会拿到空 context（追踪与超时全丢）
	c := newCtx("POST", "/api/v1/auth/login")
	var hasCtx bool
	runChain([]app.HandlerFunc{TraceID()}, func(c *app.RequestContext) {
		_, hasCtx = c.Get(CtxKey)
	}, c)
	if !hasCtx {
		t.Fatal("TraceID 未把ctx 写入 request context")
	}
}

func TestRecoveryCatchesPanic(t *testing.T) {
	c := newCtx("GET", "/x")
	var reachedAfter bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Recovery 未兜住 panic: %v", r)
			}
		}()
		runChain([]app.HandlerFunc{Recovery()}, func(*app.RequestContext) {
			panic("boom")
		}, c)
	}()
	_ = reachedAfter
	if c.Response.StatusCode() != consts.StatusInternalServerError {
		t.Fatalf("status = %d, 期望 500", c.Response.StatusCode())
	}
}

// ctxOfForTest 复现 handler 侧的 ctx 读取方式
func ctxOfForTest(c *app.RequestContext) context.Context {
	if v, ok := c.Get(CtxKey); ok {
		if ctx, ok := v.(context.Context); ok && ctx != nil {
			return ctx
		}
	}
	return context.Background()
}
