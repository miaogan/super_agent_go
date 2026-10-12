// obs.go —— gateway 的可观测中间件（HTTP 指标 + 入口追踪 span）。
//
// gateway 是全链路的第一个 span：它的 traceparent 经
// pkg/tracing.ClientMiddleware 传给下游 RPC，
// 瀑布图形态：gateway.HTTP → agent/Chat → llm/Chat（→ rag/Retrieve）。
package middleware

import (
	"context"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/aegis/pkg/metrics"
	"github.com/aegis/pkg/tracing"
)

// Metrics HTTP 指标埋点：请求数 + 耗时。
//
// path 标签用 FullPath（路由模板，如 /api/v1/agents/:agent_id/chat），
// 而非实际路径 —— 否则每个 agent_id 都会成为独立时间线。
func Metrics() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		start := time.Now()
		c.Next(ctx)

		path := c.FullPath()
		if path == "" {
			// 404 等未匹配路由：用实际路径首段聚合，避免任意路径刷爆标签
			path = "unmatched"
		}
		metrics.RecordHTTP(
			string(c.Request.Method()),
			path,
			statusLabel(c),
			time.Since(start),
		)
	}
}

// statusLabel 把状态码归并为低基数标签。
// 4xx/5xx 保留精确码（排障需要），2xx/3xx 统一为 "2xx"/"3xx"：
// 成功路径不需要细分，而成功恰恰是基数最大的部分。
func statusLabel(c *app.RequestContext) string {
	code := c.Response.StatusCode()
	if code >= 200 && code < 400 {
		return strconv.Itoa(code/100) + "xx"
	}
	return strconv.Itoa(code)
}

// Trace 入口追踪：从 HTTP 头提取 W3C 上下文并开启服务端 span。
//
// 必须注册在 Metrics 之后无依赖关系，但应在最外层
// （TraceID 之后）—— 这样后续日志可携带 trace_id 关联。
func Trace() app.HandlerFunc {
	prop := propagation.TraceContext{}

	return func(ctx context.Context, c *app.RequestContext) {
		ctx = prop.Extract(ctx, &headerCarrier{c})

		path := c.FullPath()
		if path == "" {
			path = string(c.Request.URI().Path())
		}
		sctx, span := tracing.Tracer.Start(ctx, "gateway "+string(c.Request.Method())+" "+path,
			oteltrace.WithSpanKind(oteltrace.SpanKindServer))

		c.Next(sctx)

		if code := c.Response.StatusCode(); code >= 500 {
			span.SetStatus(codes.Error, "HTTP "+strconv.Itoa(code))
		}
		span.End()
	}
}

// headerCarrier 把 Hertz 请求头适配成 otel 文本载体
type headerCarrier struct{ c *app.RequestContext }

func (h *headerCarrier) Get(key string) string {
	return string(h.c.Request.Header.Peek(key))
}
func (h *headerCarrier) Keys() []string {
	var keys []string
	h.c.Request.Header.VisitAll(func(k, _ []byte) {
		keys = append(keys, string(k))
	})
	return keys
}
func (h *headerCarrier) Set(string, string) {}
