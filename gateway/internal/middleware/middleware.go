// Package middleware 提供网关的通用中间件。
//
// 关键约束：
//
//	-鉴权必须在最前面，且失败时立刻返回，
//	  不能让请求下沉到 handler 才被拒绝
//	- SSE 路由必须豁免响应缓冲与压缩，否则流式会被攒批下发
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	"github.com/aegis/pkg/identity"
	"github.com/aegis/pkg/logger"
)

const (
	// IdentityKey 身份在 Hertz request context 中的键
	IdentityKey = "aegis.identity"
	// CtxKey 携带身份与追踪 ID 的 context 在 Hertz request context 中的键
	CtxKey = "aegis.ctx"
	// TraceIDKey 追踪 ID 在 Hertz request context 中的键
	TraceIDKey = "aegis.trace_id"
)

// TokenVerifier 校验令牌并返回调用方身份。
// 由 main 注入 auth_service 客户端实现，避免 middleware 直接依赖 RPC。
type TokenVerifier interface {
	Verify(ctx context.Context, accessToken, requiredScope string) (*commonv1.Identity, error)
}

// Auth 中间件：解析 Bearer 令牌并注入身份。
//
// ★ 为什么不自己解析 JWT：
//
//	密钥轮换、吊销名单（登出即失效）、issuer/audience 校验
//	都集中在 auth_service 一处。网关自行解析必然出现
//	"网关认为有效、后端认为无效"的分歧。
func Auth(v TokenVerifier, requiredScope string) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		token := ExtractToken(c)
		if token == "" {
			abort(c, consts.StatusUnauthorized, "UNAUTHORIZED", "缺少访问令牌")
			return
		}

		id, err := v.Verify(ctx, token, requiredScope)
		if err != nil {
			// ★ 不把验证器的原始错误透出：
			//   内部错误可能包含密钥配置等细节。
			logger.Warn(ctx, "token verify failed", "err", err)
			abort(c, consts.StatusUnauthorized, "TOKEN_INVALID", "访问令牌无效或已过期")
			return
		}

		c.Set(IdentityKey, id)
		// ★ 必须走 identity.With，不能自造 context key：
		//   handler 层用 identity.From 读取。两边key 不一致时
		//   身份会被静默丢弃（表现为所有下游收到 uid=0），
		//   而且不报错——最难查的一类bug。
		ctx = identity.With(ctx, id)
		c.Set(CtxKey, ctx)
		c.Next(ctx)
	}
}

// IdentityFrom 从 context 取身份，不存在返回匿名身份。
//
// 只是 identity.From 的语义化别名，明确"网关入口"这一来源。
func IdentityFrom(ctx context.Context) *commonv1.Identity { return identity.From(ctx) }

// ExtractToken 从请求中提取访问令牌。
//
// 支持两种来源：
//   - Authorization: Bearer <token>（标准）
//   - ?access_token=（仅用于 SSE —— EventSource API 无法设置请求头）
func ExtractToken(c *app.RequestContext) string {
	auth := string(c.Request.Header.Get("Authorization"))
	if auth != "" {
		// 大小写不敏感地匹配 Bearer 前缀
		if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
			return strings.TrimSpace(auth[7:])
		}
		return ""
	}
	// SSE 兜底：EventSource 不支持自定义请求头，只能走 query
	return string(c.Query("access_token"))
}

// OptionalAuth 尝试解析身份但不强制要求。
//
// 用于「登录与否都能访问、但登录后体验不同」的端点
// （如 Agent 列表：登录用户看自己的，游客看公共的）。
func OptionalAuth(v TokenVerifier) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if token := ExtractToken(c); token != "" {
			if id, err := v.Verify(ctx, token, ""); err == nil {
				c.Set(IdentityKey, id)
				ctx = identity.With(ctx, id)
			}
		}
		c.Set(CtxKey, ctx)
		c.Next(ctx)
	}
}

// RequireScope 要求当前身份具备指定 scope（RBAC 细粒度控制）。
//
// 必须挂在 Auth 之后：Auth 负责把身份放进 context，
// 本中间件只做授权判断。scope 来自 JWT（登录时由角色展开，
// 如 admin 角色 → "admin" scope），因此校验无需回源查询。
//
// 语义：401 = 没登录/令牌无效（Auth 拦），403 = 登录了但权限不足（本中间件拦）。
func RequireScope(scope string) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		id := identity.From(ctx)
		for _, s := range id.GetScopes() {
			// "*" 是超管通配，与 identity.IsAdmin 的语义保持一致
			if s == scope || s == "*" {
				c.Next(ctx)
				return
			}
		}
		logger.Warn(ctx, "scope 校验失败", "uid", id.GetUid(),
			"required", scope, "scopes", id.GetScopes(),
			"path", string(c.Request.URI().Path()))
		abort(c, consts.StatusForbidden, "FORBIDDEN", "无权限执行该操作")
	}
}

// Recovery panic 兜底中间件。
//
// 网关是所有流量的入口，一个 handler 的 panic
// 不应让整个进程崩溃并带走其它连接。
func Recovery() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error(ctx, "panic recovered",
					"error", r, "path", string(c.Request.URI().Path()))
				abort(c, consts.StatusInternalServerError, "INTERNAL", "服务内部错误")
			}
		}()
		c.Next(ctx)
	}
}

// RequestLog 访问日志
func RequestLog() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		start := time.Now()
		c.Next(ctx)

		// ★ SSE 路由耗时天然很长（等于整个回答时长），
		//   单独打个标记，否则会被误判为慢请求。
		path := string(c.Request.URI().Path())
		attrs := []any{
			"method", string(c.Request.Method()),
			"path", path,
			"status", c.Response.StatusCode(),
			"cost_ms", time.Since(start).Milliseconds(),
		}
		if strings.HasSuffix(path, "/stream") {
			logger.Info(ctx, "http sse", attrs...)
			return
		}
		logger.Info(ctx, "http access", attrs...)
	}
}

// CORS 跨域中间件。
func CORS(allowOrigins []string, maxAge time.Duration) app.HandlerFunc {
	allowAll := len(allowOrigins) == 0
	allowed := make(map[string]bool, len(allowOrigins))
	for _, o := range allowOrigins {
		if o == "*" {
			allowAll = true
		}
		allowed[o] = true
	}

	return func(ctx context.Context, c *app.RequestContext) {
		origin := string(c.Request.Header.Get("Origin"))
		if origin != "" && (allowAll || allowed[origin]) {
			// ★ 必须取指针：Response.Header 内含nocopy 锁，
			//   按值赋值会拷贝锁状态（go vet 会报 copylocks）。
			h := &c.Response.Header
			if allowAll {
				// 回显具体 origin 而非 *：
				// 配合 Allow-Credentials 时浏览器会拒绝 `*`。
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Vary", "Origin")
			} else {
				h.Set("Access-Control-Allow-Origin", origin)
			}
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers",
				"Origin, Content-Type, Accept, Authorization, X-Request-Id, Last-Event-ID")
			h.Set("Access-Control-Expose-Headers", "X-Request-Id, X-Trace-Id")
			if maxAge > 0 {
				h.Set("Access-Control-Max-Age", strconv.Itoa(int(maxAge.Seconds())))
			}
		}

		if string(c.Request.Method()) == consts.MethodOptions {
			// 预检请求就地终止，不下沉到 handler
			c.AbortWithStatus(consts.StatusNoContent)
			return
		}
		c.Next(ctx)
	}
}

// abort 中止请求并返回统一错误格式
func abort(c *app.RequestContext, status int, code, msg string) {
	c.AbortWithStatusJSON(status, utils.H{
		"error": utils.H{"code": code, "message": msg},
	})
}

// TraceID 为每个请求注入追踪 ID
func TraceID() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		tid := string(c.Request.Header.Get("X-Request-Id"))
		if tid == "" {
			tid = newTraceID()
		}
		c.Response.Header.Set("X-Request-Id", tid)
		c.Set(TraceIDKey, tid)
		// ★ 必须写入 CtxKey：免鉴权路由（如 /api/v1/auth/login）
		//   不经过 Auth 中间件，若只有 TraceID 在链上传 ctx 而不落盘，
		//   handler里的 ctxOf 会拿到空 context —— 追踪 ID 与超时设置全部丢失。
		next := logger.WithTraceID(ctx, tid)
		c.Set(CtxKey, next)
		c.Next(next)
	}
}

func newTraceID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源异常时退化为时间戳，不因追踪 ID 阻断请求
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// NewEngine 构造 Hertz 引擎。
//
// ★ WriteTimeout 固定为 0：
//
//	SSE 是长连接，默认的写超时会从传输层把流掐断，
//	客户端看到的是"回答到一半就没了"，且很难联想到是网关超时。
//	改由 handler 层用 context超时控制流式整体时长。
func NewEngine(addr string, read, write, idle time.Duration) *server.Hertz {
	opts := []config.Option{
		server.WithHostPorts(addr),
		// 兜底上限，防止连接堆积拖垮进程；不作用于 SSE
		server.WithReadTimeout(read),
		server.WithIdleTimeout(idle),
	}
	if write > 0 {
		opts = append(opts, server.WithWriteTimeout(write))
	}
	// else: 不设 WriteTimeout =永不超时

	return server.New(opts...)
}
