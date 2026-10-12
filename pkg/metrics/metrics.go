// Package metrics 提供基于 Prometheus 的指标采集。
//
// 设计要点：
//   - 全局默认 Registry：服务进程内唯一，健康端口 /metrics 直接暴露
//   - Kitex server/client 中间件：RPC 维度请求数、耗时、在途量
//   - RecordHTTP：gateway（Hertz）HTTP 维度请求数、耗时
//   - 运行时指标（go_*、process_*）由 client_golang 默认注册
//
// 为什么不用 kitex-contrib 的 prometheus 扩展：
//
//	指标命名与标签维度希望全系统统一收敛到 aegis_* 前缀，
//	且中间件要同时覆盖 gRPC 与 TTHeader 两条传输路径 ——
//	自写 endpoint.Middleware 与 pkg/internalauth 同构，行为可控。
package metrics

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/expfmt"

	"github.com/aegis/pkg/errno"
)

// 全局 Registry：所有服务的指标统一挂这里，/metrics 一次性暴露。
var registry = prometheus.NewRegistry()

func init() {
	// Go 运行时与进程指标（内存/GC/goroutine、CPU/句柄/常驻内存）
	registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
}

// Handler 返回 /metrics 处理器（挂标准库 net/http 服务，如健康端口）。
func Handler() http.Handler {
	// OpenMetrics 协商：支持 Prometheus 2.x+ 的文本协议，禁 gzip 避免
	// 小抓取场景的额外开销。指标端点不含敏感数据但也不鉴权 ——
	// 调用方必须只把它挂在内部端口（health HTTP）上。
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// ContentTypeText Prometheus 文本协议的 Content-Type
var ContentTypeText = string(expfmt.NewFormat(expfmt.TypeTextPlain))

// WriteText 以 Prometheus 文本协议渲染全部指标。
//
// 供非 net/http 框架（Hertz）直接桥接 —— RequestContext 不满足
// 标准 http.ResponseWriter，与其走兼容适配器不如直接渲染文本：
// Prometheus 服务端对 text 格式的抓取完全兼容。
func WriteText(w io.Writer) error {
	mfs, err := registry.Gather()
	if err != nil {
		return err
	}
	enc := expfmt.NewEncoder(w, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return err
		}
	}
	return nil
}

// ---------- RPC 指标 ----------

var (
	rpcServerRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aegis_rpc_server_requests_total",
			Help: "RPC 请求数（服务端视角）",
		},
		[]string{"rpc_service", "method", "code"},
	)
	rpcServerLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "aegis_rpc_server_request_duration_seconds",
			Help:    "RPC 处理耗时分布（服务端视角）",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		},
		[]string{"rpc_service", "method"},
	)
	rpcServerActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "aegis_rpc_server_active_requests",
			Help: "当前在途 RPC 请求数（服务端视角）",
		},
		[]string{"rpc_service"},
	)
	rpcClientRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aegis_rpc_client_requests_total",
			Help: "RPC 请求数（调用方视角）",
		},
		[]string{"caller", "rpc_service", "method", "code"},
	)
	rpcClientLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "aegis_rpc_client_request_duration_seconds",
			Help:    "RPC 端到端耗时分布（调用方视角，含网络）",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		},
		[]string{"caller", "rpc_service", "method"},
	)

	// HTTP 指标（gateway 用，方法见 RecordHTTP）
	httpRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aegis_http_requests_total",
			Help: "HTTP 请求数",
		},
		[]string{"method", "path", "code"},
	)
	httpLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "aegis_http_request_duration_seconds",
			Help:    "HTTP 处理耗时分布",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300},
		},
		[]string{"method", "path"},
	)
)

func init() {
	registry.MustRegister(
		rpcServerRequests, rpcServerLatency, rpcServerActive,
		rpcClientRequests, rpcClientLatency,
		httpRequests, httpLatency,
	)
}

// rpcCode 把错误映射为可聚合的短码：成功 / 业务错误码。
// 直接用完整错误消息当标签会造成基数爆炸（用户输入进了 message）；
// 错误码是有限集合（errno 包定义），可安全作为标签。
// 非业务错误统一落在 50001（ErrInternal）。
func rpcCode(err error) string {
	return strconv.FormatInt(int64(errno.CodeOf(err)), 10)
}

// ---------- Kitex 中间件 ----------

// KitexServerMiddleware 服务端指标中间件。
//
// serviceName 用调用方声明的本服务名（cfg.Service.Name），
// 避免 RPCInfo 在错误路径上可能取不到服务名导致标签缺失。
func KitexServerMiddleware(serviceName string) endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp interface{}) (err error) {
			method := "unknown"
			if ri := rpcinfo.GetRPCInfo(ctx); ri != nil && ri.Invocation() != nil {
				if m := ri.Invocation().MethodName(); m != "" {
					method = m
				}
			}

			rpcServerActive.WithLabelValues(serviceName).Inc()
			defer func() {
				rpcServerActive.WithLabelValues(serviceName).Dec()
			}()

			start := time.Now()
			err = next(ctx, req, resp)
			elapsed := time.Since(start).Seconds()

			rpcServerRequests.WithLabelValues(serviceName, method, rpcCode(err)).Inc()
			rpcServerLatency.WithLabelValues(serviceName, method).Observe(elapsed)
			return err
		}
	}
}

// KitexClientMiddleware 调用方视角指标中间件。
//
// caller 为本服务名，rpc_service 取自 RPCInfo.To()（下游服务）。
// 把「调用方视角耗时」与「服务端视角耗时」分开：
//   - client 高、server 低 → 网络排队/连接建立问题
//   - 两者都高 → 下游本身慢
//     这个二分法是排查跨服务延迟最常用的第一刀。
func KitexClientMiddleware(caller string) endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp interface{}) (err error) {
			method, peer := "unknown", "unknown"
			if ri := rpcinfo.GetRPCInfo(ctx); ri != nil {
				if ri.Invocation() != nil {
					if m := ri.Invocation().MethodName(); m != "" {
						method = m
					}
				}
				if ri.To() != nil {
					if s := ri.To().ServiceName(); s != "" {
						peer = s
					}
				}
			}

			start := time.Now()
			err = next(ctx, req, resp)
			elapsed := time.Since(start).Seconds()

			rpcClientRequests.WithLabelValues(caller, peer, method, rpcCode(err)).Inc()
			rpcClientLatency.WithLabelValues(caller, peer, method).Observe(elapsed)
			return err
		}
	}
}

// RecordHTTP gateway HTTP 指标埋点。
//
// path 必须传路由模板（如 /api/v1/agents/:agent_id/chat）而非实际路径，
// 否则 agent_id 会成为高基数标签把时序库打爆。
func RecordHTTP(method, path, code string, elapsed time.Duration) {
	httpRequests.WithLabelValues(method, path, code).Inc()
	httpLatency.WithLabelValues(method, path).Observe(elapsed.Seconds())
}
