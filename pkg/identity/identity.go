// Package identity 提供调用方身份的上下文透传。
//
// 链路：gateway 解析 JWT → 构造 commonv1.Identity → 写入 context
//
//	→ 下游服务 handler 直接读取，无需重复解析令牌。
//
// 设计原因：Identity 走 proto 字段（而非 context）是显式契约；
// context 版本仅为减少 handler 样板而存在，两者由本包桥接。
package identity

import (
	"context"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	"github.com/cloudwego/kitex/pkg/endpoint"
)

type ctxKey struct{}

// With 把 Identity 写入 context
func With(ctx context.Context, id *commonv1.Identity) context.Context {
	if id == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// From 从 context 读取 Identity；不存在返回匿名身份
func From(ctx context.Context) *commonv1.Identity {
	if ctx == nil {
		return &commonv1.Identity{}
	}
	if v, ok := ctx.Value(ctxKey{}).(*commonv1.Identity); ok && v != nil {
		return v
	}
	return &commonv1.Identity{}
}

// FromReq 优先从 ctx 读，ctx 空则回退到 req（handler 入口推荐写法）
func FromReq(ctx context.Context, id *commonv1.Identity) *commonv1.Identity {
	if From(ctx).GetUid() > 0 || len(From(ctx).GetScopes()) > 0 {
		return From(ctx)
	}
	return id
}

// Uid 返回当前调用方 uid，0 表示匿名
func Uid(ctx context.Context) int64 { return From(ctx).GetUid() }

// Tenant 返回当前调用方租户
func Tenant(ctx context.Context) string { return From(ctx).GetTenant() }

// IsAdmin 判断是否具备 admin 角色
func IsAdmin(ctx context.Context) bool {
	for _, r := range From(ctx).GetScopes() {
		if r == "admin" || r == "*" {
			return true
		}
	}
	return false
}

// Anonymous 是否匿名调用
func Anonymous(ctx context.Context) bool { return Uid(ctx) == 0 }

// IdentityProvider 有 Identity 字段的请求体实现此接口。
type IdentityProvider interface {
	GetIdentity() *commonv1.Identity
}

// ServerMiddleware Kitex server 中间件：尝试从 req.Identity 注入 ctx。
// ★ 注意：Kitex server middleware 在 v0.16 里对 protobuf unary RPC 的 req 是 *streaming.Args 包装，
//
//	  无法直接 type assertion。推荐 handler 入口显式调用
//
//		ctx = identity.With(ctx, req.GetIdentity())
//
//	  或者用 FromReq(ctx, req.GetIdentity()) 做兜底。
func ServerMiddleware() endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			if p, ok := req.(IdentityProvider); ok {
				if id := p.GetIdentity(); id != nil {
					ctx = With(ctx, id)
				}
			}
			return next(ctx, req, resp)
		}
	}
}
