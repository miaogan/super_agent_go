package retrieve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/kitex/client/callopt"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"
	llmservice "github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/vectordb"
	"github.com/aegis/pkg/vectordb/mockstore"
	"github.com/aegis/pkg/websearch"

	"github.com/aegis/rag_service/internal/meta"
)

// ---------- 多来源夹具 ----------

type hybridFixture struct {
	engine *Engine
	llm    *hybridLLM
	kbID   int64
	docID  int64
}

// hybridLLM 可编程 LLM（embed 可注入失败）。
type hybridLLM struct {
	embedErr error
}

var _ llmservice.Client = (*hybridLLM)(nil)

func (h *hybridLLM) Embed(_ context.Context, _ *llmv1.EmbedReq,
	_ ...callopt.Option) (*llmv1.EmbedResp, error) {
	if h.embedErr != nil {
		return nil, h.embedErr
	}
	return &llmv1.EmbedResp{
		Dimension:  1024,
		Embeddings: []*llmv1.Embedding{{Vector: vec1024}},
	}, nil
}

func (h *hybridLLM) Rerank(context.Context, *llmv1.RerankReq, ...callopt.Option) (*llmv1.RerankResp, error) {
	return nil, errors.New("stub 未实现")
}
func (h *hybridLLM) Health(context.Context, *commonv1.HealthReq, ...callopt.Option) (*commonv1.HealthResp, error) {
	return nil, errors.New("stub 未实现")
}
func (h *hybridLLM) Chat(context.Context, *llmv1.ChatReq, ...callopt.Option) (*llmv1.ChatResp, error) {
	return nil, errors.New("stub 未实现")
}
func (h *hybridLLM) ChatStream(context.Context, *llmv1.ChatReq, ...callopt.Option) (llmservice.LLMService_ChatStreamClient, error) {
	return nil, errors.New("stub 未实现")
}
func (h *hybridLLM) ListModels(context.Context, *llmv1.ListModelsReq, ...callopt.Option) (*llmv1.ListModelsResp, error) {
	return nil, errors.New("stub 未实现")
}
func (h *hybridLLM) GetCapabilities(context.Context, *llmv1.GetCapabilitiesReq, ...callopt.Option) (*llmv1.GetCapabilitiesResp, error) {
	return nil, errors.New("stub 未实现")
}
func (h *hybridLLM) GetUsage(context.Context, *llmv1.GetUsageReq, ...callopt.Option) (*llmv1.GetUsageResp, error) {
	return nil, errors.New("stub 未实现")
}

// newHybridFixture 造一个三来源齐全的检索环境。
//
// cfg 传入的 RAGConf 需带 EnableKeywordSearch/EnableWebSearch；
// 这里固定注入 mockstore + mock 网络检索。
func newHybridFixture(t *testing.T, cfg conf.RAGConf) *hybridFixture {
	t.Helper()
	ms := meta.NewMemoryStore()
	vec := mockstore.New()
	llm := &hybridLLM{}

	ctx := context.Background()
	kbID, _ := ms.NextKBID(ctx)
	if err := ms.CreateKB(ctx, &ragv1.KnowledgeBase{KbId: kbID, Name: "混合库"}); err != nil {
		t.Fatalf("建知识库失败: %v", err)
	}
	docID, _ := ms.NextDocID(ctx)
	if err := ms.CreateDoc(ctx, &meta.DocumentRecord{
		Doc: &ragv1.Document{DocId: docID, KbId: kbID, FileName: "hybrid.txt"},
	}); err != nil {
		t.Fatalf("建文档失败: %v", err)
	}

	// KB 内容与 web 语料**刻意不同**，便于区分来源命中
	contents := []string{
		"知识库内部文档：关于产品发布计划与内部流程说明。",
		"第二段内部内容：季度目标与团队分工安排。",
	}
	chunks := make([]*ragv1.Chunk, 0, len(contents))
	vecChunks := make([]*vectordb.Chunk, 0, len(contents))
	for i, c := range contents {
		cid := ms.NextChunkID(docID, i)
		chunks = append(chunks, &ragv1.Chunk{ChunkId: cid, KbId: kbID, DocId: docID, Seq: int32(i), Content: c})
		vecChunks = append(vecChunks, &vectordb.Chunk{ChunkID: cid, KBID: kbID, DocID: docID, Vector: vec1024})
	}
	if err := ms.UpdateChunks(ctx, docID, chunks); err != nil {
		t.Fatalf("写切片失败: %v", err)
	}
	if err := vec.EnsureCollection(ctx, "aegis_kb_"+itoa(kbID), 1024); err != nil {
		t.Fatalf("建集合失败: %v", err)
	}
	if err := vec.Upsert(ctx, "aegis_kb_"+itoa(kbID), vecChunks); err != nil {
		t.Fatalf("写向量失败: %v", err)
	}

	cfg.DefaultScoreThreshold = 0.01 // mock 向量阈值放低，让命中稳定
	cfg.RerankEnabled = false
	e := New(Options{
		Meta:         ms,
		Vector:       vec,
		LLM:          llm,
		RAGConfig:    cfg,
		MilvusConfig: conf.MilvusConf{Dim: 1024, Timeout: 3 * time.Second},
		EmbedModel:   "test-embed",
		WebSearch:    websearch.NewMockProvider(),
	})
	return &hybridFixture{engine: e, llm: llm, kbID: kbID, docID: docID}
}

// retrieve 便捷调用。
func (f *hybridFixture) retrieve(t *testing.T, q string) *Result {
	t.Helper()
	res, err := f.engine.Retrieve(context.Background(), &Request{
		Query: q, KBIDs: []int64{f.kbID}, TopK: 5, Debug: true,
	})
	if err != nil {
		t.Fatalf("Retrieve 失败: %v", err)
	}
	return res
}

// ---------- 用例 ----------

// TestHybridFusionIncludesWeb 三来源融合：结果同时含 KB 命中与 web 命中。
func TestHybridFusionIncludesWeb(t *testing.T) {
	f := newHybridFixture(t, conf.RAGConf{
		EnableKeywordSearch: true,
		EnableWebSearch:     true,
		WebSearch:           conf.WebSearchConf{Provider: "mock", TopK: 5},
	})
	// 查询命中 web 语料（"RAG 检索增强生成"）而非 KB 内容
	res := f.retrieve(t, "RAG 检索增强生成是什么")

	if res.Degraded {
		t.Fatalf("多来源正常不应降级: %s", res.DegradeReason)
	}
	hasWeb := false
	hasKB := false
	for _, h := range res.Hits {
		if strings.HasPrefix(h.GetChunkId(), "web:") {
			hasWeb = true
			if h.GetDocTitle() == "" || h.GetContent() == "" {
				t.Error("web 命中应带标题与摘要")
			}
		} else {
			hasKB = true
		}
	}
	if !hasWeb {
		t.Errorf("结果应含 web 命中: %+v", res.Hits)
	}
	if !hasKB {
		t.Errorf("结果应含 KB 命中（向量+关键词）: %+v", res.Hits)
	}
}

// TestHybridVectorFailureStillReturns 防单源①：向量来源故障，关键词/网络仍出结果。
func TestHybridVectorFailureStillReturns(t *testing.T) {
	f := newHybridFixture(t, conf.RAGConf{
		EnableKeywordSearch: true,
		EnableWebSearch:     true,
		WebSearch:           conf.WebSearchConf{Provider: "mock", TopK: 5},
	})
	f.llm.embedErr = errors.New("embedding 服务不可用")

	res := f.retrieve(t, "内部文档 季度目标")
	if res.Degraded {
		t.Errorf("向量单来源故障不应整体降级（防单源）: %s", res.DegradeReason)
	}
	if len(res.Hits) == 0 {
		t.Fatal("向量故障时关键词/网络仍应产出结果")
	}
}

// TestHybridWebOnlySurvives 防单源②：向量+关键词都不可用，网络来源单独存活。
//
// 构造：keyword 来源关闭 + 向量来源故障（embedding 失败）→ 仅 web 存续，
// 检索仍返回结果且不降级 —— 这是"多来源防单源"的极端存续证明。
func TestHybridWebOnlySurvives(t *testing.T) {
	f := newHybridFixture(t, conf.RAGConf{
		EnableKeywordSearch: false,
		EnableWebSearch:     true,
		WebSearch:           conf.WebSearchConf{Provider: "mock", TopK: 5},
	})
	f.llm.embedErr = errors.New("embedding 不可用")

	res := f.retrieve(t, "RAG 检索增强生成指南")
	if res.Degraded {
		t.Errorf("网络来源单独存活不应降级: %s", res.DegradeReason)
	}
	if len(res.Hits) == 0 {
		t.Fatal("仅剩网络来源时仍应产出结果")
	}
	for _, h := range res.Hits {
		if !strings.HasPrefix(h.GetChunkId(), "web:") {
			t.Errorf("仅网络来源时命中应全部来自 web: %q", h.GetChunkId())
		}
	}
}

// TestHybridAllSourcesFailed 全部来源失败 → 整体降级 + 空结果。
func TestHybridAllSourcesFailed(t *testing.T) {
	f := newHybridFixture(t, conf.RAGConf{
		EnableKeywordSearch: true,
		EnableWebSearch:     true,
		WebSearch:           conf.WebSearchConf{Provider: "mock", TopK: 5},
	})
	f.llm.embedErr = errors.New("embedding 不可用")

	// 空查询：embed 报"检索词为空"、关键词/网络无命中 → 三路全无产出
	res := f.retrieve(t, "")
	if !res.Degraded {
		t.Error("全部来源失败必须置 degraded")
	}
	if !strings.Contains(res.DegradeReason, "all_sources_failed") {
		t.Errorf("降级原因应含 all_sources_failed: %q", res.DegradeReason)
	}
	if len(res.Hits) != 0 {
		t.Errorf("全部失败应返回空: %d 条", len(res.Hits))
	}
}

// TestVectorOnlyStrategy vector_only 策略：仅向量，web/关键词不参与融合。
func TestVectorOnlyStrategy(t *testing.T) {
	f := newHybridFixture(t, conf.RAGConf{
		MergeStrategy:       conf.MergeStrategyVectorOnly,
		EnableKeywordSearch: true,
		EnableWebSearch:     true,
		WebSearch:           conf.WebSearchConf{Provider: "mock", TopK: 5},
	})
	// 查询命中 KB（向量）
	res := f.retrieve(t, "内部文档 季度目标")

	if res.Degraded {
		t.Fatalf("vector_only 不应降级: %s", res.DegradeReason)
	}
	for _, h := range res.Hits {
		if strings.HasPrefix(h.GetChunkId(), "web:") {
			t.Errorf("vector_only 策略不应出现 web 命中: %q", h.GetChunkId())
		}
	}
}

// TestHybridKeywordSourceEnrich 关键词常驻：术语命中的切片能进入融合结果。
func TestHybridKeywordSourceEnrich(t *testing.T) {
	f := newHybridFixture(t, conf.RAGConf{
		EnableKeywordSearch: true,
		EnableWebSearch:     false,
	})
	// 查询含 KB 内容里的精确术语
	res := f.retrieve(t, "产品发布计划 内部流程")
	if res.Degraded {
		t.Errorf("关键词常驻不应降级: %s", res.DegradeReason)
	}
	if len(res.Hits) == 0 {
		t.Error("关键词来源应有命中")
	}
}
