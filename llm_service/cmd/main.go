// Command llm_service 大模型统一出口。
//
// 全系统唯一调用模型的入口：agent_service 与 rag_service 都经由此处，
// 因此计量、熔断、降级、Embedding 缓存集中在这里实现。
package main

import (
	"context"
	"fmt"
	"os"

	llmservice "github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	"github.com/cloudwego/kitex/server"

	"github.com/aegis/llm_service/internal/providerfactory"
	"github.com/aegis/llm_service/internal/service"
	"github.com/aegis/llm_service/internal/toolresolver"
	"github.com/aegis/llm_service/internal/usagestore"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/serverx"
)

func main() {
	ctx := context.Background()

	if err := serverx.Start(serverx.Options{
		ServiceName:     "llm_service",
		DefaultRPCPort:  8884,
		DefaultHTTPPort: 8094,
		Register: func(svr server.Server, app *serverx.App) (func(context.Context) error, error) {
			c := app.Cfg

			// Provider 选择见 providerfactory：mock 模式强制离线，
			// 否则按 llm.provider 选 eino / rawhttp。
			// cleanup 交由 serverx 在优雅退出阶段调用，此处转交即可。
			prov, provCleanup, err := providerfactory.Build(ctx, c, app.MockMode())
			if err != nil {
				return nil, fmt.Errorf("构建 Provider 失败: %w", err)
			}

			// ToolResolver：PG 只读查 tools 表，补全 JSON Schema。
			// 工具解析不是热点，连接池 MaxConns=2 足够。
			var (
				toolRes      service.ToolResolver
				toolResClose func()
			)
			if !app.MockMode() {
				if tr, err := toolresolver.NewPG(ctx, c.Postgres); err == nil {
					toolRes = tr
					toolResClose = func() { tr.Close() }
					logger.Info(ctx, "llm_service: ToolResolver 已注入（PG 只读）")
				} else {
					logger.Warn(ctx, "llm_service: ToolResolver 未注入（工具调用将被忽略）",
						"err", err)
				}
			}

			// 用量落库：让 token_usage 表成为用量的真相源。
			// 不注入时 UsageRecorder 只有进程内环形缓冲 —— 重启即丢历史，
			// 且多副本各自一份，用量统计口径不成立。
			// 与 ToolResolver 同一套降级姿势：存不下用量不该让服务起不来。
			var (
				usageSink   usagestore.Sink
				usageFinish func(context.Context) error
			)
			if !app.MockMode() {
				if us, err := usagestore.NewPG(ctx, c.Postgres); err == nil {
					usageSink = us
					usageFinish = us.Close
					logger.Info(ctx, "llm_service: 用量落库已注入（PG 异步批量）")
				} else {
					logger.Warn(ctx, "llm_service: 用量只留内存（重启即丢、多副本不聚合）",
						"err", err)
				}
			}

			llmservice.RegisterService(svr, service.New(service.Options{
				Provider:           prov,
				Mock:               app.FaultInjector(),
				CacheEnabled:       c.LLM.CacheEnabled,
				CacheTTL:           c.LLM.EmbedCacheTTL,
				DefaultModel:       c.LLM.DefaultModel,
				DefaultEmbedModel:  c.LLM.DefaultEmbedModel,
				DefaultEmbedDim:    c.LLM.DefaultEmbedDim,
				DefaultRerankModel: c.RAG.RerankModel,
				MaxTokens:          c.LLM.MaxTokens,
				ToolResolver:       toolRes,
				UsageSink:          usageSink,
			}))

			caps := prov.Capabilities()
			logger.Info(ctx, "llm_service ready",
				"provider", prov.Name(),
				"default_model", c.LLM.DefaultModel,
				"embed_model", c.LLM.DefaultEmbedModel,
				"embed_dim", c.LLM.DefaultEmbedDim,
				"rerank_model", c.RAG.RerankModel,
				"tool_resolver", toolRes != nil,
				"capabilities", map[string]bool{
					"tool_call":  caps.ToolCall,
					"reasoning":  caps.Reasoning,
					"streaming":  caps.Streaming,
					"embeddings": caps.Embeddings,
				})

			// cleanup：退出时释放 Provider 的 HTTP 连接 + ToolResolver PG 池
			return func(ctx context.Context) error {
				if provCleanup != nil {
					_ = provCleanup(ctx)
				} else {
					_ = prov.Close()
				}
				if toolResClose != nil {
					toolResClose()
				}
				// 用量出口自带连接池，与其他清理项无先后依赖；
				// Close 内部会停止接收、把缓冲区里剩下的批次排空再关池。
				// 给它 ctx 是为了在优雅退出期限内别卡太久。
				if usageFinish != nil {
					_ = usageFinish(ctx)
				}
				return nil
			}, nil
		},
	}); err != nil {
		logger.Error(ctx, "llm_service 启动失败", "err", err)
		os.Exit(1)
	}
}
