// Command agent_service 智能体编排服务。
//
// 编排链路：
//
//	gateway → agent_service → llm_service（模型出口）
//	                       → rag_service（知识库检索）
//
// 设计要点：
//   - agent_service 不直接持有任何厂商 SDK，模型能力全部经llm_service，
//     与 rag_service 保持一致：厂商密钥只存在于 llm_service 一处
//   - 会话与消息持久化双实现：PG（生产）+ 内存（mock / PG 不可用兜底），
//     service/orchestrate 依赖接口，装配只发生在本文件
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	agentservice "github.com/aegis/idl/kitex_gen/agentv1/agentservice"
	llmservice "github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragservice "github.com/aegis/idl/kitex_gen/ragv1/ragservice"
	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/server"

	"github.com/aegis/pkg/internalauth"
	"github.com/aegis/pkg/kitexx"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/metrics"
	"github.com/aegis/pkg/serverx"
	"github.com/aegis/pkg/tracing"

	"github.com/aegis/agent_service/internal/orchestrate"
	"github.com/aegis/agent_service/internal/service"
	"github.com/aegis/agent_service/internal/store"
	"github.com/aegis/agent_service/internal/toolexec"
)

func main() {
	ctx := context.Background()

	if err := serverx.Start(serverx.Options{
		ServiceName:     "agent_service",
		DefaultRPCPort:  8886,
		DefaultHTTPPort: 8096,
		Register: func(svr server.Server, app *serverx.App) (func(context.Context) error, error) {
			c := app.Cfg

			// 1) 会话仓储：PG 优先，mock / 不可用时回退内存
			var (
				st      store.Repository
				cleanup func(context.Context) error
			)
			newMemory := func() store.Repository {
				ms := store.New(store.Options{
					MaxMessagesPerSession: c.Agent.MaxMessagesPerSession,
					MaxSessionsPerUser:    c.Agent.MaxSessionsPerUser,
					SessionTTL:            c.Agent.SessionTTL,
				})
				// 定期清理空闲会话：内存实现必须设上限，
				// 否则长时间运行的进程会被历史会话吃光内存。
				stopJanitor := ms.StartJanitor(time.Hour)
				cleanup = func(context.Context) error { stopJanitor(); return nil }
				return ms
			}

			switch {
			case app.MockMode():
				st = newMemory()
				logger.Info(ctx, "agent_service: mock 模式，内存仓储")
			default:
				pg, err := store.NewPgStore(ctx, c.Postgres)
				if err != nil {
					logger.Warn(ctx, "agent_service: PG 不可用，回退内存仓储（数据重启即丢）",
						"dsn", maskDSN(c.Postgres.DSN), "err", err)
					st = newMemory()
				} else {
					st = pg
					cleanup = func(context.Context) error { pg.Close(); return nil }
					logger.Info(ctx, "agent_service: PG 仓储已连接",
						"dsn_masked", maskDSN(c.Postgres.DSN))
				}
			}

			// 2) 下游客户端
			llm, err := newLLMClient(c.Upstream.LLMService, c.Internal.Token)
			if err != nil {
				cleanup(context.Background())
				return nil, fmt.Errorf("连接 llm_service 失败: %w", err)
			}
			rag, err := newRAGClient(c.Upstream.RAGService, c.Internal.Token)
			if err != nil {
				cleanup(context.Background())
				return nil, fmt.Errorf("连接 rag_service 失败: %w", err)
			}

			// 3) 编排器
				var toolExec toolexec.ToolExecutor
				if reg, ok := st.(toolexec.ToolRegistry); ok {
					toolExec = toolexec.NewHTTP(reg, 30*time.Second)
					logger.Info(ctx, "agent_service: 工具执行器已注入", "registry", fmt.Sprintf("%T", reg))
				}

				orch := orchestrate.New(orchestrate.Options{
					LLM:           &orchestrate.LLMAdapter{C: llm},
					RAG:           &orchestrate.RAGAdapter{C: rag},
					KBMount:       &orchestrate.RAGAdapter{C: rag},
					Store:         st,
					Agent:         c.Agent,
					RAGCfg:        c.RAG,
					EnableRAG:     c.Agent.EnableRAG,
					ToolExec:      toolExec,
					MaxToolRounds: 5,
				})

			agentservice.RegisterService(svr, service.New(st, orch, app.FaultInjector()))

			logger.Info(ctx, "agent_service ready",
				"llm_service", c.Upstream.LLMService,
				"rag_service", c.Upstream.RAGService,
				"default_model", c.Agent.DefaultModel,
				"history_turns", c.Agent.HistoryTurns,
				"max_context_chars", c.Agent.MaxContextChars,
				"rag_enabled", c.Agent.EnableRAG,
				"store", fmt.Sprintf("%T", st))

			return cleanup, nil
		},
	}); err != nil {
		logger.Error(ctx, "agent_service 启动失败", "err", err)
		os.Exit(1)
	}
}

// newLLMClient 构造 llm_service 客户端
func newLLMClient(addr, internalToken string) (llmservice.Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("upstream.llm_service 未配置")
	}
	// 冷启动时下游 DNS 可能尚未注册：WithHostPorts 解析失败会把地址
	// 永久标记为 unix 网络（见 pkg/kitexx.WaitHostPort 注释）
	if err := kitexx.WaitHostPort(addr, 30*time.Second); err != nil {
		return nil, err
	}
	opts := []client.Option{
		client.WithHostPorts(addr),
		client.WithRPCTimeout(120 * time.Second), // 流式回答可能较长
	}
	opts = append(opts, internalauth.ClientOptions(internalToken)...)
	// 追踪 + 指标：traceparent 双通道注入与调用方视角耗时统计
	opts = append(opts, client.WithMiddleware(tracing.ClientMiddleware()))
	opts = append(opts, client.WithMiddleware(metrics.KitexClientMiddleware("agent_service")))
	return llmservice.NewClient("llm_service", opts...)
}

// newRAGClient 构造 rag_service 客户端（含内部认证令牌，见 pkg/internalauth）
func newRAGClient(addr, internalToken string) (ragservice.Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("upstream.rag_service 未配置")
	}
	if err := kitexx.WaitHostPort(addr, 30*time.Second); err != nil {
		return nil, err
	}
	opts := []client.Option{
		client.WithHostPorts(addr),
		client.WithRPCTimeout(30 * time.Second),
	}
	opts = append(opts, internalauth.ClientOptions(internalToken)...)
	opts = append(opts, client.WithMiddleware(tracing.ClientMiddleware()))
	opts = append(opts, client.WithMiddleware(metrics.KitexClientMiddleware("agent_service")))
	return ragservice.NewClient("rag_service", opts...)
}

// maskDSN 把 DSN 里的 password 替换为 ****，仅用于日志
func maskDSN(dsn string) string {
	if i := strings.Index(dsn, ":"); i >= 0 {
		if j := strings.Index(dsn[i:], "@"); j >= 0 {
			return dsn[:i+1] + "****" + dsn[i+j:]
		}
	}
	return dsn
}
