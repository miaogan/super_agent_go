// Command gateway 统一 HTTP 入口。
//
// 职责边界（刻意收窄）：
//   - 只做 HTTP ⇄ RPC 转换、鉴权、限流与错误映射
//   - 不持有任何业务逻辑：编排在 agent_service，检索在 rag_service
//   - 不直连模型：厂商密钥只存在于 llm_service 一处，
//     网关硬探 llm_service 会触发真实推理与计费
//
// 为什么用 Hertz 而非 Kitex 的 HTTP 层：
//
//	SSE 长连接需要在响应头设置后立即 Flush，并在流式期间保活。
//	Hertz 的 RequestContext 直接暴露 Response 的 Flush 能力，
//	配合官方 pkg/protocol/sse 可以避免手写帧格式 reinvent。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	agentservice "github.com/aegis/idl/kitex_gen/agentv1/agentservice"
	authservice "github.com/aegis/idl/kitex_gen/authv1/authservice"
	"github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragservice "github.com/aegis/idl/kitex_gen/ragv1/ragservice"
	userservice "github.com/aegis/idl/kitex_gen/userv1/userservice"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/kitex/client"

	"github.com/aegis/pkg/internalauth"
	"github.com/aegis/pkg/kitexx"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/metrics"
	"github.com/aegis/pkg/serverx"
	"github.com/aegis/pkg/tracing"

	"github.com/aegis/gateway/internal/handler"
	"github.com/aegis/gateway/internal/middleware"
	"github.com/aegis/gateway/internal/router"
)

func main() {
	if err := serverx.Start(serverx.Options{
		ServiceName:     "gateway",
		DefaultHTTPPort: 8080,
		HTTPOnly:        true,
		HTTPServer:      startHTTP,
	}); err != nil {
		logger.Error(context.Background(), "gateway 启动失败", "err", err)
		os.Exit(1)
	}
}

// startHTTP 装配下游客户端、路由与 Hertz 实例
func startHTTP(app *serverx.App) (func(context.Context) error, error) {
	c := app.Cfg
	gw := c.Gateway

	// ---- 下游客户端 ----
	// 内部认证令牌：非空时每次 RPC 经 TTHeader 携带，下游 server 校验；
	// 追踪与指标中间件随行：traceparent 双通道注入 + 调用方视角耗时统计
	iaOpts := append(internalauth.ClientOptions(c.Internal.Token),
		client.WithMiddleware(tracing.ClientMiddleware()),
		client.WithMiddleware(metrics.KitexClientMiddleware("gateway")),
	)

	// 冷启动时下游 DNS 可能尚未注册：WithHostPorts 解析失败会把地址
	// 永久标记为 unix 网络（见 pkg/kitexx.WaitHostPort 注释）
	for _, addr := range []string{
		c.Upstream.AuthService, c.Upstream.UserService,
		c.Upstream.AgentService, c.Upstream.RAGService,
		c.Upstream.LLMService,
	} {
		if err := kitexx.WaitHostPort(addr, 30*time.Second); err != nil {
			return nil, err
		}
	}

	auth, err := authservice.NewClient("auth_service",
		append(iaOpts,
			client.WithHostPorts(c.Upstream.AuthService),
			client.WithRPCTimeout(timeoutOr(gw.RPCTimeout, 5*time.Second)),
		)...,
	)
	if err != nil {
		return nil, fmt.Errorf("连接 auth_service 失败: %w", err)
	}
	usr, err := userservice.NewClient("user_service",
		append(iaOpts,
			client.WithHostPorts(c.Upstream.UserService),
			client.WithRPCTimeout(timeoutOr(gw.RPCTimeout, 5*time.Second)),
		)...,
	)
	if err != nil {
		return nil, fmt.Errorf("连接 user_service 失败: %w", err)
	}
	ag, err := agentservice.NewClient("agent_service",
		append(iaOpts,
			client.WithHostPorts(c.Upstream.AgentService),
			// 普通 RPC 超时由网关控制；流式另有 handler 层的整体上限，
			// 因此这里的客户端超时要给流式留出余量，否则流到一半被 Kitex 掐断。
			client.WithRPCTimeout(timeoutOr(gw.RPCTimeout, 30*time.Second)),
		)...,
	)
	if err != nil {
		return nil, fmt.Errorf("连接 agent_service 失败: %w", err)
	}
	rag, err := ragservice.NewClient("rag_service",
		append(iaOpts,
			client.WithHostPorts(c.Upstream.RAGService),
			client.WithRPCTimeout(timeoutOr(gw.RPCTimeout, 30*time.Second)),
		)...,
	)
	if err != nil {
		return nil, fmt.Errorf("连接 rag_service 失败: %w", err)
	}
	llm, err := llmservice.NewClient("llm_service",
		append(iaOpts,
			client.WithHostPorts(c.Upstream.LLMService),
			client.WithRPCTimeout(timeoutOr(gw.RPCTimeout, 30*time.Second)),
		)...,
	)
	if err != nil {
		return nil, fmt.Errorf("连接 llm_service 失败: %w", err)
	}

	deps := &handler.Deps{
		Auth:          auth,
		User:          usr,
		Agent:         ag,
		RAG:           rag,
		LLM:           llm,
		Timeout:       gw.RPCTimeout,
		StreamTimeout: gw.StreamTimeout,
	}

	// ---- Hertz ----
	h := middleware.NewEngine(
		gw.Addr,
		timeoutOr(gw.ReadTimeout, 15*time.Second),
		gw.WriteTimeout, // ★ 保持 0，见 middleware.NewEngine 注释
		timeoutOr(gw.IdleTimeout, 120*time.Second),
	)

	router.Register(h, router.Options{
		Deps:           deps,
		StaticDir:      gw.StaticDir,
		AllowOrigins:   gw.AllowOrigins,
		MaxAge:         gw.MaxAge,
		MetricsEnabled: c.Metrics.Enabled,
		MetricsPath:    c.Metrics.Path,
	})

	ctx := logger.WithTraceID(context.Background(), "gateway-boot")
	go func() {
		// ★ Spin 必须在所有路由注册完成之后调用：
		//   Hertz 的路由树在 Spin 后才对外可见，
		//   注册期间到来的请求会404。
		h.Spin()

		logger.Info(ctx, "gateway ready",
			"addr", gw.Addr,
			"auth_service", c.Upstream.AuthService,
			"user_service", c.Upstream.UserService,
			"agent_service", c.Upstream.AgentService,
			"rag_service", c.Upstream.RAGService,
			"static_dir", gw.StaticDir,
			"write_timeout", gw.WriteTimeout,
			"stream_timeout", gw.StreamTimeout,
			"allow_origins", gw.AllowOrigins)
	}()

	return func(context.Context) error { return shutdown(h) }, nil
}

func shutdown(h *server.Hertz) error {
	// 给在途的 SSE 连接一个收尾窗口：
	// 立即关闭会把用户正在看的回答截断。
	return h.Shutdown(context.Background())
}

func timeoutOr(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}
