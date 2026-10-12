// kitex.go —— Kitex RPC 的追踪中间件。
//
// 传播机制（与 pkg/internalauth 同构，已经容器冒烟验证）：
//
//	client 端（双通道注入）：
//	  通道1  metainfo.WithPersistentValues   → TTHeader/thrift 路径
//	  通道2  npmeta.AppendToOutgoingContext  → gRPC/HTTP2 路径
//	         （pb 含 streaming 方法时传输协议被生成代码强制为 GRPC，
//	          metainfo 桶发不出去，必须走 nphttp2 metadata）
//
//	server 端（双键读取）：
//	  TTHeader 路径恢复原键 "traceparent"；
//	  gRPC 路径下 kitex 经 FromHTTPHeader 恢复时键被 CGI 化
//	  （小写连字符 → 大写下划线）→ "TRACEPARENT"。
package tracing

import (
	"context"
	"strings"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/remote/trans/nphttp2/metadata"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	traceparentKey    = "traceparent"
	traceparentCGIKey = "TRACEPARENT" // gRPC 路径下 metainfo 的 CGI 变量形式
)

// traceparentOf 按 W3C 规范序列化 SpanContext。
// 格式：{version}-{trace-id}-{parent-span-id}-{flags}，如
// "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
func traceparentOf(sc oteltrace.SpanContext) string {
	var b strings.Builder
	b.WriteString("00-")
	b.WriteString(sc.TraceID().String())
	b.WriteByte('-')
	b.WriteString(sc.SpanID().String())
	b.WriteByte('-')
	b.WriteString(sc.TraceFlags().String())
	return b.String()
}

// singleHeaderCarrier 把单个 W3C 头喂给标准 propagator 解析
type singleHeaderCarrier map[string]string

func (c singleHeaderCarrier) Get(key string) string { return c[key] }
func (c singleHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}
func (c singleHeaderCarrier) Set(string, string) {}

// ClientMiddleware 把当前 span 上下文注入 RPC 元数据。
//
// 上游无活动 span（如未启追踪）时不注入，下游自然成为根 span。
func ClientMiddleware() endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp interface{}) (err error) {
			if sc := oteltrace.SpanContextFromContext(ctx); sc.IsValid() {
				tp := traceparentOf(sc)
				ctx = metainfo.WithPersistentValues(ctx, traceparentKey, tp)
				ctx = metadata.AppendToOutgoingContext(ctx, traceparentKey, tp)
			}
			return next(ctx, req, resp)
		}
	}
}

// ServerMiddleware 提取上游追踪上下文并开启服务端 span。
//
// span 名使用 "{service}/{method}"，跨服务瀑布图一眼可读。
// 错误自动记录为 span 的 Error status（业务错误码在 attributes 里）。
func ServerMiddleware(serviceName string) endpoint.Middleware {
	prop := propagation.TraceContext{}

	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp interface{}) (err error) {
			// ---- 提取上游上下文 ----
			if tp, ok := extractTraceparent(ctx); ok {
				ctx = prop.Extract(ctx, singleHeaderCarrier{traceparentKey: tp})
			}

			// ---- span 命名 ----
			method := "unknown"
			if ri := rpcinfo.GetRPCInfo(ctx); ri != nil && ri.Invocation() != nil {
				if m := ri.Invocation().MethodName(); m != "" {
					method = m
				}
			}

			// 每次调用动态取包级 Tracer：noop 切换在运行期生效
			sctx, span := Tracer.Start(ctx, serviceName+"/"+method,
				oteltrace.WithSpanKind(oteltrace.SpanKindServer))

			err = next(sctx, req, resp)

			if err != nil {
				span.SetStatus(codes.Error, err.Error())
			}
			span.End()
			return err
			// 注意：返回值沿用外层 err（sctx 不向上传递——
			// 服务端 span 不需要再传给响应方，biz handler 拿到的
			// ctx 也无需携带服务端 span，避免无谓的上下文替换）
		}
	}
}

// extractTraceparent 依次尝试三个来源：
//  1. persistent 桶原键（TTHeader 路径）
//  2. persistent 桶 CGI 键（gRPC 路径，FromHTTPHeader 键名转换）
//  3. nphttp2 incoming metadata 直读（兜底）
func extractTraceparent(ctx context.Context) (string, bool) {
	if v, ok := metainfo.GetPersistentValue(ctx, traceparentKey); ok && v != "" {
		return v, true
	}
	if v, ok := metainfo.GetPersistentValue(ctx, traceparentCGIKey); ok && v != "" {
		return v, true
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(traceparentKey); len(v) > 0 && v[0] != "" {
			return v[0], true
		}
	}
	return "", false
}
