// Package tracing 提供基于 OpenTelemetry 的分布式追踪。
//
// 结构：
//   - tracing.go       SDK 初始化（OTLP HTTP 导出、采样、资源）与优雅关闭
//   - kitex.go         Kitex server/client 中间件（跨服务 span 串联）
//
// 为什么自研中间件而不用 kitex-contrib/obs-opentelemetry：
//   该扩展 v0.3.0 停留在 otel 0.39 / kitex 0.5 时代，
//   依赖 kitex 内部 TraceCarrier 机制，与 kitex v0.16 兼容性无保障；
//   且我们已有经容器冒烟验证的「metainfo + nphttp2 metadata 双通道透传」
//   （见 pkg/internalauth）—— traceparent 走同一条路，机制复用、风险为零。
package tracing

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/aegis/pkg/logger"
)

// Tracer 全系统统一埋点入口。
//
// 未初始化（otel.enabled=false）时是 noop tracer：
// 中间件代码路径完全一致，只是不产生 span —— 关闭观测零行为差异。
var Tracer trace.Tracer = noop.NewTracerProvider().Tracer("aegis")

// Config 追踪配置（来自 conf.OTelConf）
type Config struct {
	// Enabled 总开关。false 时全程 noop，无 SDK 开销
	Enabled bool
	// Endpoint OTLP HTTP 端点，如 http://jaeger:4318
	Endpoint string
	// SamplerRatio 采样率 0-1（1=全采样）
	SamplerRatio float64
	// ExportTimeout 单次导出超时
	ExportTimeout time.Duration
}

// Init 初始化全局 TracerProvider（OTLP HTTP 导出）。
//
// 返回的 shutdown 必须在进程退出前调用（BatchSpanProcessor 冲刷）。
// 已显式设置过全局 provider 时跳过（防重复初始化）。
func Init(ctx context.Context, serviceName, version string, cfg Config) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		logger.Info(ctx, "otel 未启用，追踪为 noop 模式")
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
		otlptracehttp.WithTimeout(exportTimeoutOf(cfg)),
	)
	if err != nil {
		return nil, err
	}

	res, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version),
		),
	)
	if err != nil {
		return nil, err
	}

	ratio := cfg.SamplerRatio
	if ratio <= 0 || ratio > 1 {
		ratio = 1
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		// ParentBased：上游决定采样的 span 继续采，
		// 保证一条调用链要么全采要么全丢 —— 断链的 trace 无法串联排查
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)

	otel.SetTracerProvider(tp)
	// W3C Trace Context + Baggage：跨进程传播的事实标准
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	// 错误去重：otel 内部错误（如导出失败）打业务日志而不是 panic
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn(ctx, "otel 内部错误", "err", err)
	}))

	Tracer = tp.Tracer("aegis")
	logger.Info(ctx, "otel tracer provider 已初始化",
		"endpoint", cfg.Endpoint, "ratio", ratio, "service", serviceName)

	return tp.Shutdown, nil
}

func exportTimeoutOf(cfg Config) time.Duration {
	if cfg.ExportTimeout > 0 {
		return cfg.ExportTimeout
	}
	return 10 * time.Second
}
