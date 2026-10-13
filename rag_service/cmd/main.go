// Command rag_service 知识库与检索服务。
//
// 数据分治（ADR-006）：
//   - 元数据（知识库/文档/切片正文）：PostgreSQL，内容真相源
//   - 向量与检索标量：Milvus 3.0.2（经自研 pkg/vectordb 适配层）
//
// Embedding 不直连厂商，统一经 llm_service.Embed —— 这样厂商密钥
// 只存在于 llm_service 一处。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	llmservice "github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragservice "github.com/aegis/idl/kitex_gen/ragv1/ragservice"
	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/internalauth"
	"github.com/aegis/pkg/kitexx"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/metrics"
	"github.com/aegis/pkg/minio"
	"github.com/aegis/pkg/parser"
	"github.com/aegis/pkg/serverx"
	"github.com/aegis/pkg/tracing"
	"github.com/aegis/pkg/vectordb"
	"github.com/aegis/pkg/vectordb/milvus3"
	"github.com/aegis/pkg/vectordb/mockstore"
	"github.com/aegis/pkg/websearch"
	"github.com/aegis/rag_service/internal/ingest"
	"github.com/aegis/rag_service/internal/meta"
	"github.com/aegis/rag_service/internal/retrieve"
	"github.com/aegis/rag_service/internal/service"
	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/server"
)

// buildWebSearch 依据配置构造网络检索来源。
//
//   - provider=mock：确定性假结果（无 key 环境 / 冒烟测试）
//   - provider=http：调用真实搜索 API（Endpoint 必填，复用 pkg/fetch 防 SSRF）
//   - 未启用或未配置：返回 nil（检索引擎自动跳过该来源）
//
// buildPDFEngine 按配置装配 PDF 正文提取引擎。
//
// 返回 nil 表示用 pkg/parser 的本地实现。
//
// ★ pdf_parser=mineru 但 base_url 缺失时在此报错退出：
//
//	让服务带着"以为在用 MinerU、实则无 OCR"的状态启动，
//	代价是大量文档静默抽出空正文，比启动失败难排查一个量级。
func buildPDFEngine(ctx context.Context, cfg conf.RAGConf) (parser.PDFEngine, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.PDFParser)) {
	case "", "local":
		logger.Info(ctx, "rag_service: PDF 解析走本地实现（无 OCR，扫描件会解析失败）")
		return nil, nil
	case "mineru":
		if strings.TrimSpace(cfg.MinerU.BaseURL) == "" {
			return nil, fmt.Errorf("rag.pdf_parser=mineru 但 rag.mineru.base_url 未配置")
		}
		eng, err := parser.NewMinerU(parser.MinerUConfig{
			BaseURL:          cfg.MinerU.BaseURL,
			Timeout:          cfg.MinerU.Timeout,
			MaxResponseBytes: int64(cfg.MinerU.MaxResponseMB) << 20,
			AuthToken:        cfg.MinerU.AuthToken,
		})
		if err != nil {
			return nil, fmt.Errorf("构造 MinerU 客户端失败: %w", err)
		}
		logger.Info(ctx, "rag_service: PDF 解析走 MinerU",
			"endpoint", cfg.MinerU.BaseURL, "timeout", cfg.MinerU.Timeout)
		return eng, nil
	default:
		return nil, fmt.Errorf("rag.pdf_parser 取值非法: %q（支持 local / mineru）", cfg.PDFParser)
	}
}

func buildWebSearch(cfg conf.RAGConf) (websearch.Provider, error) {
	if !cfg.EnableWebSearch || cfg.WebSearch.Provider == "" {
		return nil, nil
	}
	switch cfg.WebSearch.Provider {
	case "mock":
		return websearch.NewMockProvider(), nil
	case "http":
		if cfg.WebSearch.Endpoint == "" {
			return nil, fmt.Errorf("web_search.provider=http 时必须配置 Endpoint")
		}
		return websearch.NewHTTPProvider(websearch.HTTPOptions{
			Endpoint: cfg.WebSearch.Endpoint,
			APIKey:   cfg.WebSearch.APIKey,
			Timeout:  cfg.WebSearch.Timeout,
			TopK:     cfg.WebSearch.TopK,
		})
	default:
		return nil, fmt.Errorf("未知 web_search.provider=%q（可选：mock / http）", cfg.WebSearch.Provider)
	}
}

func main() {
	ctx := context.Background()

	if err := serverx.Start(serverx.Options{
		ServiceName:     "rag_service",
		DefaultRPCPort:  8885,
		DefaultHTTPPort: 8095,
		Register: func(svr server.Server, app *serverx.App) (func(context.Context) error, error) {
			c := app.Cfg

			// 1) 元数据存储：PG 优先（内容真相源），mock / 不可用回退内存
			var metaStore meta.Store
			var closePg func()
			if app.MockMode() {
				metaStore = meta.NewMemoryStore()
				logger.Info(ctx, "mock mode: 内存元数据存储")
			} else if pg, err := meta.NewPgStore(ctx, c.Postgres); err != nil {
				// 与 agent_service 同策略：PG 挂了服务仍可启动（检索降级链
				// 与 ingest 均依赖元数据，内存实现仅保 mock 冒烟可用，
				// 数据重启即丢 —— 必须在日志里显式告警，不能静默降级）
				logger.Warn(ctx, "PG 不可用，回退内存元数据存储（数据重启即丢）",
					"err", err)
				metaStore = meta.NewMemoryStore()
			} else {
				metaStore = pg
				// pg.Close 不能 defer 在本回调里：Register 返回即触发，
				// 会把刚建好的连接池关掉（首个请求即 closed pool）。
				// 挂到 Register 返回的 cleanup，由 serverx 在退出时调用。
				closePg = func() { pg.Close() }
				logger.Info(ctx, "PG 元数据存储已连接")
			}

			// 2) 向量存储
			//    mock 模式 → 内存实现（无 Milvus 依赖，冒烟用）
			//    生产 → pkg/vectordb/milvus3（自研 Milvus 3.0适配层，ADR-006）
			var vectorStore vectordb.Store
			var mockStoreInst *mockstore.Store
			// Milvus 连接的 Close 也必须挂到 Register 返回的 cleanup：
			// defer 在本回调 return 时立即执行，会把刚建好的客户端关掉，
			// 后续所有 Milvus RPC 报 "not connected"（SDK 的 Close 会把
			// 内部 service 置 nil，且没有任何自动重连）。
			closeVector := func() error { return nil }
			if app.MockMode() {
				mockStoreInst = mockstore.New()
				vectorStore = mockStoreInst
				closeVector = func() error { return vectorStore.Close() }
				logger.Info(ctx, "mock mode: 使用内存向量库", "dim", c.Milvus.Dim)
			} else {
				mvs, err := milvus3.New(ctx, c.Milvus)
				if err != nil {
					// ★ 这里必须让服务启动失败，而不是降级为内存库：
					//   静默换成 mock 会让「向量库配错了」表现为
					//   「检索结果差了点」——直到用户投诉才发现真相。
					//   连接问题在运行期才暴露，那时由 Ping/Search
					//   转成 ErrUnavailable 触发关键词检索降级（见 retrieve/engine.go）。
					return nil, fmt.Errorf("连接 Milvus 失败: %w", err)
				}
				vectorStore = mvs
				closeVector = func() error {
					if cerr := mvs.Close(); cerr != nil {
						logger.Warn(ctx, "Milvus 关闭异常", "err", cerr)
					}
					return nil
				}
				logger.Info(ctx, "Milvus 已连接",
					"addr", c.Milvus.Addr, "db", c.Milvus.DBName,
					"collection", c.Milvus.Collection, "dim", c.Milvus.Dim)
			}

			// 3) 对象存储（RAG 文件原始副本，可降级）
			//    优先连接 MinIO；失败不阻断服务启动 —— UploadDocument
			//    会记录 warn 并走内存直存（RawBytes 落 PG raw_bytes 列）
			var minioStore minio.Store
			if app.MockMode() {
				logger.Info(ctx, "mock mode: 跳过 MinIO 连接")
			} else {
				if ms, err := minio.New(ctx, c.Minio); err != nil {
					logger.Warn(ctx, "MinIO 不可用，文件原始副本不落盘", "err", err)
				} else {
					minioStore = ms
					logger.Info(ctx, "MinIO 已连接",
						"endpoint", c.Minio.Endpoint, "bucket", c.Minio.Bucket)
				}
			}

			// 4) llm_service 客户端（向量化统一走此出口）
			llm, err := newLLMClient(c.Upstream.LLMService, c.Internal.Token)
			if err != nil {
				return nil, fmt.Errorf("连接 llm_service 失败: %w", err)
			}

			// 4) 入库流水线与检索引擎
			pdfEngine, err := buildPDFEngine(ctx, c.RAG)
			if err != nil {
				return nil, err
			}
			pipeline := ingest.New(metaStore, vectorStore, llm, c.RAG, pdfEngine)
			webProvider, err := buildWebSearch(c.RAG)
			if err != nil {
				return nil, fmt.Errorf("构造网络检索来源失败: %w", err)
			}
			engine := retrieve.New(retrieve.Options{
				Meta:         metaStore,
				Vector:       vectorStore,
				LLM:          llm,
				RAGConfig:    c.RAG,
				MilvusConfig: c.Milvus,
				EmbedModel:   c.LLM.DefaultEmbedModel,
				MockMode:     app.MockMode(),
				WebSearch:    webProvider,
			})

			// 5) 补偿任务：定期重试失败的向量化
			bgCtx, cancelBg := context.WithCancel(ctx)
			pipeline.StartCompensator(bgCtx, c.RAG.CompensateInterval)

			ragservice.RegisterService(svr, service.New(metaStore, vectorStore, pipeline, engine,
				app.FaultInjector(), c.RAG, minioStore,
				service.EmbedDefaults{
					Model: c.LLM.DefaultEmbedModel,
					Dim:   c.LLM.DefaultEmbedDim,
				}))

			kwEnabled, webEnabled := c.RAG.SourcesEnabled()
			logger.Info(ctx, "rag_service ready",
				"llm_service", c.Upstream.LLMService,
				"embed_model", c.LLM.DefaultEmbedModel,
				"dim", c.Milvus.Dim,
				"merge_strategy", c.RAG.EffectiveMergeStrategy(),
				"keyword_source", kwEnabled,
				"web_source", webEnabled,
				"compensate_interval", c.RAG.CompensateInterval.String())

			return func(context.Context) error {
				cancelBg()
				if closePg != nil {
					closePg()
				}
				return closeVector()
			}, nil
		},
	}); err != nil {
		logger.Error(ctx, "rag_service 启动失败", "err", err)
		os.Exit(1)
	}
}

// newLLMClient 构造 llm_service 客户端（含内部认证令牌，见 pkg/internalauth）
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
		client.WithRPCTimeout(60 * time.Second),
	}
	opts = append(opts, internalauth.ClientOptions(internalToken)...)
	// 追踪 + 指标：traceparent 双通道注入与调用方视角耗时统计
	opts = append(opts, client.WithMiddleware(tracing.ClientMiddleware()))
	opts = append(opts, client.WithMiddleware(metrics.KitexClientMiddleware("rag_service")))
	return llmservice.NewClient("llm_service", opts...)
}
