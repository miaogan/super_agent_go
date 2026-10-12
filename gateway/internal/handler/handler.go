// Package handler 实现网关的 HTTP handler。
//
// 职责：
//   - HTTP ⇄ pb 转换
//   - 调用下游 RPC
//   - 统一错误响应
//
// 两条硬约束：
//  1. 身份只从 context 取（由中间件解析令牌得到），
//     绝不信任请求体里的 uid/tenant ——
//     否则任何人改个 uid 就能读别人的会话。
//  2. 下游错误映射为合适的 HTTP 状态码，
//     不把内部错误原样抛给客户端（可能泄露密钥配置等细节）。
package handler

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	agentservice "github.com/aegis/idl/kitex_gen/agentv1/agentservice"
	authservice "github.com/aegis/idl/kitex_gen/authv1/authservice"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	llmservice "github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragservice "github.com/aegis/idl/kitex_gen/ragv1/ragservice"
	userservice "github.com/aegis/idl/kitex_gen/userv1/userservice"

	"github.com/aegis/gateway/internal/middleware"
	"github.com/aegis/pkg/identity"
	"github.com/aegis/pkg/logger"
)

// Deps 下游依赖集合
type Deps struct {
	Auth  authservice.Client
	User  userservice.Client
	Agent agentservice.Client
	RAG   ragservice.Client
	LLM   llmservice.Client

	// Timeout 普通 RPC 超时（流式走各自的更长超时）
	Timeout time.Duration
	// StreamTimeout 流式整体超时
	StreamTimeout time.Duration
	// MaxBodyBytes 请求体上限；<=0 时用 4MB。
	//
	// 必须在应用层限：Hertz 默认不限制请求体大小，
	// 一个超大上传会把网关内存吃光。
	MaxBodyBytes int64
}

// OK 返回成功响应
func OK(c *app.RequestContext, data any) {
	c.JSON(consts.StatusOK, utils.H{"data": data})
}

// H 把窄签名 handler 适配成 Hertz 的 app.HandlerFunc。
//
// Hertz 的 HandlerFunc 签名是 func(ctx, c)，但业务 handler
// 拿到的 ctx 只能通过中间件写入的 c.Get(middleware.CtxKey) 拿（handler 内部
// 统一走 ctxOf），直接写成方法值既不匹配也容易让两套 ctx 各走一半。
func H(fn func(c *app.RequestContext)) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		// 兜底写入：中间件链之外的路径（如健康检查内联 handler）
		// 也保证 ctxOf 拿到的是带超时/追踪的 ctx。
		if _, ok := c.Get(middleware.CtxKey); !ok {
			c.Set(middleware.CtxKey, ctx)
		}
		fn(c)
	}
}

// Error 返回统一错误格式
func Error(c *app.RequestContext, status int, code, msg string) {
	c.AbortWithStatusJSON(status, utils.H{
		"error": utils.H{"code": code, "message": msg},
	})
}

// ctx 返回携带身份的 context。
//
// 优先复用中间件写入的 ctx（已含追踪 ID），
// 否则退化为后台 context —— 保证身份一致且链路可追踪。
func ctxOf(c *app.RequestContext) context.Context {
	if v, ok := c.Get(middleware.CtxKey); ok {
		if ctx, ok := v.(context.Context); ok && ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

// idOf 取当前调用方身份
func idOf(c *app.RequestContext) *commonv1.Identity {
	return identity.From(ctxOf(c))
}

// withTimeout 为 RPC 附加超时
func (d *Deps) withTimeout(c *app.RequestContext) (context.Context, context.CancelFunc) {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return context.WithTimeout(ctxOf(c), timeout)
}

// streamTimeout 流式超时
func (d *Deps) streamTimeout() time.Duration {
	if d.StreamTimeout <= 0 {
		return 10 * time.Minute
	}
	return d.StreamTimeout
}

// extractBearer 从 Authorization 头提取 Bearer 令牌。
// 格式不符时返回空串，绝不"宽容"地取整串当令牌。
func extractBearer(header string) string {
	h := strings.TrimSpace(header)
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// logError 记录下游错误并返回客户端
//
// ★ 只把操作名返回给用户，内部错误详情仅进日志 ——
//
//	错误里常含连接地址、配置片段等不该外泄的信息。
func logError(ctx context.Context, c *app.RequestContext, op string, err error) {
	logger.Error(ctx, "下游调用失败", "op", op, "err", err)
	Error(c, consts.StatusBadGateway, "UPSTREAM_ERROR", "服务暂时不可用，请稍后重试")
}

// defaultMaxBodyBytes 请求体默认上限
const defaultMaxBodyBytes = 4 << 20 // 4MB

// page 解析分页参数
//
// page_size 上限 200：不分页上限的话
// 一个 ?page_size=1000000 就能让下游一次拉全表。
func page(c *app.RequestContext) *commonv1.PageReq {
	size := queryInt(c, "page_size", 20)
	if size <= 0 {
		size = 20
	}
	if size > 200 {
		size = 200
	}
	p := queryInt(c, "page", 1)
	if p <= 0 {
		p = 1
	}
	return &commonv1.PageReq{Page: int32(p), PageSize: int32(size)}
}

// queryInt 解析整数查询参数，非法时返回默认值
func queryInt(c *app.RequestContext, key string, def int) int {
	v := c.Query(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// queryInt64 解析 int64 查询参数
func queryInt64(c *app.RequestContext, key string) int64 {
	v := c.Query(key)
	if v == "" {
		return 0
	}
	// ★ 必须用 ParseInt 而非复用 queryInt：
	//   后者按 int 累加，既会溢出（负数直接变成正数，
	//   如 "-5" 解析成 5），也会在 32 位平台截断。
	//   负数被读成正数意味着 -1 会变成"最后一个用户"。
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// paramInt64 解析路径参数（路由声明的 :name）。
func paramInt64(c *app.RequestContext, key string) int64 {
	v := c.Param(key)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// queryStr 解析字符串查询参数
func queryStr(c *app.RequestContext, key string) string {
	return c.Query(key)
}

// firstNonEmptyStr 返回首个非空字符串
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ownerFilter 从请求中解析"看别人的资源"过滤条件。
//
// ★ tenant 永远取自身身份，不读query。
//
//	租户隔离是安全边界而不是查询条件：
//	允许 ?tenant=other 越权读到别家数据。
//	需要跨租户运营时走独立的管理员接口并强制校验 scope。
func ownerFilter(c *app.RequestContext) (ownerUID int64, tenant string) {
	id := idOf(c)
	return queryInt64(c, "owner_uid"), id.GetTenant()
}
