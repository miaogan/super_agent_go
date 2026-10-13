package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/kitex/client/callopt"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"
	llmservice "github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/mockx"
	"github.com/aegis/pkg/vectordb"
	"github.com/aegis/pkg/vectordb/mockstore"

	"github.com/aegis/rag_service/internal/meta"
)

// stubLLM 可编程的 llmservice.Client：按文本自动生成确定性向量。
type stubLLM struct {
	embedDim   int32
	embedErr   error
	lastReq    *llmv1.EmbedReq
	embedCalls int
}

var _ llmservice.Client = (*stubLLM)(nil)

func (s *stubLLM) Embed(_ context.Context, req *llmv1.EmbedReq,
	_ ...callopt.Option) (*llmv1.EmbedResp, error) {
	s.embedCalls++
	s.lastReq = req
	if s.embedErr != nil {
		return nil, s.embedErr
	}
	dim := s.embedDim
	if dim <= 0 {
		dim = 1024
	}
	resp := &llmv1.EmbedResp{Dimension: dim, Model: req.GetModel()}
	for i, text := range req.GetTexts() {
		resp.Embeddings = append(resp.Embeddings, &llmv1.Embedding{
			Index:  int32(i),
			Vector: mockx.FakeEmbedding(int(dim), text),
		})
	}
	return resp, nil
}

func (s *stubLLM) Health(context.Context, *commonv1.HealthReq, ...callopt.Option) (*commonv1.HealthResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubLLM) Chat(context.Context, *llmv1.ChatReq, ...callopt.Option) (*llmv1.ChatResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubLLM) ChatStream(context.Context, *llmv1.ChatReq, ...callopt.Option) (llmservice.LLMService_ChatStreamClient, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubLLM) ListModels(context.Context, *llmv1.ListModelsReq, ...callopt.Option) (*llmv1.ListModelsResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubLLM) GetCapabilities(context.Context, *llmv1.GetCapabilitiesReq, ...callopt.Option) (*llmv1.GetCapabilitiesResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubLLM) GetUsage(context.Context, *llmv1.GetUsageReq, ...callopt.Option) (*llmv1.GetUsageResp, error) {
	return nil, errors.New("stub 未实现")
}
func (s *stubLLM) Rerank(context.Context, *llmv1.RerankReq, ...callopt.Option) (*llmv1.RerankResp, error) {
	return nil, errors.New("stub 未实现")
}

// ---------- 夹具 ----------

// pipelineFixture 组合内存元数据 + mock 向量库 + 可编程 LLM。
type pipelineFixture struct {
	pipe  *Pipeline
	ms    *meta.MemoryStore
	vec   *mockstore.Store
	llm   *stubLLM
	kb    *ragv1.KnowledgeBase
	docID int64
}

// newFixture 造一个可跑 Ingest 的环境。
func newFixture(t *testing.T) *pipelineFixture {
	t.Helper()
	ms := meta.NewMemoryStore()
	vec := mockstore.New()
	llm := &stubLLM{embedDim: 1024}
	cfg := conf.RAGConf{IngestWorkers: 1, CompensateBatch: 100}

	ctx := context.Background()
	kbID, err := ms.NextKBID(ctx)
	if err != nil {
		t.Fatalf("分配 kb_id 失败: %v", err)
	}
	kb := &ragv1.KnowledgeBase{
		KbId:         kbID,
		Name:         "测试库",
		ChunkSize:    50,
		ChunkOverlap: 10,
		EmbeddingDim: 1024,
		EmbedModel:   "test-embed",
	}
	if err := ms.CreateKB(ctx, kb); err != nil {
		t.Fatalf("建知识库失败: %v", err)
	}
	docID, err := ms.NextDocID(ctx)
	if err != nil {
		t.Fatalf("分配 doc_id 失败: %v", err)
	}

	return &pipelineFixture{
		// nil PDF 引擎 = 走 pkg/parser 的本地实现
		pipe:  New(ms, vec, llm, cfg, nil),
		ms:    ms,
		vec:   vec,
		llm:   llm,
		kb:    kb,
		docID: docID,
	}
}

// seedDoc 落一个文档记录（模拟 rag.go 在调 Ingest 前的 CreateDoc）。
func (f *pipelineFixture) seedDoc(t *testing.T, content string) *meta.DocumentRecord {
	t.Helper()
	rec := &meta.DocumentRecord{
		Doc: &ragv1.Document{
			DocId: f.docID, KbId: f.kb.KbId,
			FileName: "t.txt", FileType: "txt",
		},
		RawBytes: []byte(content),
	}
	if err := f.ms.CreateDoc(context.Background(), rec); err != nil {
		t.Fatalf("建文档失败: %v", err)
	}
	return rec
}

// ---------- 纯函数 ----------

// TestSplitEmpty 空文本返回 nil。
func TestSplitEmpty(t *testing.T) {
	if got := split("   \n\t", 10, 2, func(i int) string { return "c" }); got != nil {
		t.Errorf("空文本应返回 nil, 实际 %d 条", len(got))
	}
}

// TestSplitDefaults size<=0 回落 800；overlap>=size 回落到 size/4。
func TestSplitDefaults(t *testing.T) {
	text := strings.Repeat("中文测试内容", 200) // 1000 rune
	chunks := split(text, 0, 0, func(i int) string { return "c" })
	if len(chunks) == 0 {
		t.Fatal("默认参数应能切出切片")
	}
	// overlap 修正：size=10, overlap=20 → 修正为 2
	small := split(strings.Repeat("abcd", 20), 10, 20, func(i int) string { return "c" })
	if len(small) == 0 {
		t.Fatal("overlap 修正后应能切分")
	}
}

// TestSplitHardCut 超长单段按 size 硬切，段间保留 overlap。
func TestSplitHardCut(t *testing.T) {
	// 单段 500 字符，无分隔符 → 应切成多块
	text := strings.Repeat("a", 500)
	chunks := split(text, 100, 10, func(i int) string { return "c" })
	if len(chunks) <= 1 {
		t.Fatalf("500 字符 / size=100 应切成多块, 实际 %d", len(chunks))
	}
	for i, c := range chunks {
		content := c.GetContent()
		if len([]rune(content)) > 100 {
			t.Errorf("第 %d 块 %d rune 超过 size=100", i, len([]rune(content)))
		}
	}
}

// TestSplitIDsSequential 切片 ID 应连续且与序号一致。
func TestSplitIDsSequential(t *testing.T) {
	chunks := split(strings.Repeat("段落一。段落二。段落三。", 30), 40, 5,
		func(seq int) string { return "doc-1-" + itoa32(seq) })
	for i, c := range chunks {
		if int(c.GetSeq()) != i {
			t.Errorf("第 %d 块 Seq=%d 不符", i, c.GetSeq())
		}
		if c.GetChunkId() != "doc-1-"+itoa32(i) {
			t.Errorf("第 %d 块 ID=%q 不符", i, c.GetChunkId())
		}
	}
}

func itoa32(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// TestEstimateTokens 估算应恒为正数。
func TestEstimateTokens(t *testing.T) {
	if got := estimateTokens("你好世界 hello world"); got <= 0 {
		t.Errorf("estimateTokens 应为正数, 实际 %d", got)
	}
}

// TestHashContent 确定性且内容不同哈希不同。
func TestHashContent(t *testing.T) {
	h1 := HashContent([]byte("abc"))
	h2 := HashContent([]byte("abc"))
	if h1 != h2 {
		t.Errorf("相同内容哈希应一致: %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("sha256 hex 应为 64 字符, 实际 %d", len(h1))
	}
	if HashContent([]byte("abc")) == HashContent([]byte("abd")) {
		t.Error("不同内容哈希不应相同")
	}
}

// ---------- Ingest 主流程 ----------

// TestIngestSuccess 正常入库：正文落库、向量化、状态就绪。
func TestIngestSuccess(t *testing.T) {
	f := newFixture(t)
	rec := f.seedDoc(t, "检索增强生成是让大模型先检索知识库再回答。知识库里的内容决定回答质量。")

	res, err := f.pipe.Ingest(context.Background(), f.kb, rec, nil)
	if err != nil {
		t.Fatalf("Ingest 失败: %v", err)
	}
	if res.Degraded {
		t.Errorf("成功链路不应降级: %s", res.DegradeReason)
	}
	if res.ChunkCount <= 0 || res.Vectorized != res.ChunkCount {
		t.Errorf("ChunkCount=%d Vectorized=%d 应相等且非零", res.ChunkCount, res.Vectorized)
	}
	// Embed 调用参数：document 类型 + 指定模型
	if f.llm.lastReq.GetInputType() != "document" {
		t.Errorf("向量化应使用 document 类型, 实际 %q", f.llm.lastReq.GetInputType())
	}
	if f.llm.lastReq.GetModel() != "test-embed" {
		t.Errorf("向量化应传模型, 实际 %q", f.llm.lastReq.GetModel())
	}

	// 文档状态与切片状态
	got, err := f.ms.GetDoc(context.Background(), f.docID)
	if err != nil {
		t.Fatalf("GetDoc 失败: %v", err)
	}
	if got.Doc.GetStatus() != int32(meta.DocReady) {
		t.Errorf("文档状态应为 DocReady, 实际 %d", got.Doc.GetStatus())
	}
	chunks, _ := f.ms.ListChunks(context.Background(), f.docID, 1, 100)
	for _, c := range chunks {
		if c.GetVecStatus() != int32(meta.VecReady) {
			t.Errorf("切片 %s vec_status 应为 Ready, 实际 %d", c.GetChunkId(), c.GetVecStatus())
		}
	}
	// KB 统计已回写
	kb, _ := f.ms.GetKB(context.Background(), f.kb.KbId)
	if kb.GetDocCount() != 1 || kb.GetChunkCount() != int64(res.ChunkCount) {
		t.Errorf("KB 统计未回写: doc=%d chunk=%d", kb.GetDocCount(), kb.GetChunkCount())
	}
}

// TestIngestEmptyContent 空内容应标记失败而非静默跳过。
func TestIngestEmptyContent(t *testing.T) {
	f := newFixture(t)
	rec := f.seedDoc(t, "   \n\t  ")

	_, err := f.pipe.Ingest(context.Background(), f.kb, rec, nil)
	if err == nil {
		t.Fatal("空内容应返回错误")
	}
	got, _ := f.ms.GetDoc(context.Background(), f.docID)
	if got.Doc.GetStatus() != int32(meta.DocFailed) {
		t.Errorf("空内容文档状态应为 DocFailed, 实际 %d", got.Doc.GetStatus())
	}
}

// TestIngestParseError 损坏的二进制文件应明确报错并标记失败。
func TestIngestParseError(t *testing.T) {
	f := newFixture(t)
	rec := f.seedDoc(t, "not a zip")
	rec.Doc.FileType = "docx"

	_, err := f.pipe.Ingest(context.Background(), f.kb, rec, nil)
	if err == nil {
		t.Fatal("损坏 docx 应报错")
	}
	got, _ := f.ms.GetDoc(context.Background(), f.docID)
	if got.Doc.GetStatus() != int32(meta.DocFailed) {
		t.Errorf("损坏文档状态应为 DocFailed, 实际 %d", got.Doc.GetStatus())
	}
}

// TestIngestEmbedError Embed 失败不阻断入库：降级为部分成功，正文保留。
func TestIngestEmbedError(t *testing.T) {
	f := newFixture(t)
	f.llm.embedErr = errors.New("llm timeout")
	rec := f.seedDoc(t, "正文内容先落库。即使向量化失败，正文也必须保留供补偿。")

	res, err := f.pipe.Ingest(context.Background(), f.kb, rec, nil)
	if err != nil {
		t.Fatalf("向量化失败不应让 Ingest 报错（降级语义）: %v", err)
	}
	if !res.Degraded {
		t.Error("向量化失败必须标记 degraded")
	}
	if !strings.Contains(res.DegradeReason, "embedding_failed") {
		t.Errorf("降级原因应含 embedding_failed: %q", res.DegradeReason)
	}
	got, _ := f.ms.GetDoc(context.Background(), f.docID)
	if got.Doc.GetStatus() != int32(meta.DocPartial) {
		t.Errorf("文档状态应为 DocPartial, 实际 %d", got.Doc.GetStatus())
	}
	// 正文（切片）已落库，可供检索/补偿
	chunks, total := f.ms.ListChunks(context.Background(), f.docID, 1, 100)
	if total == 0 || len(chunks) == 0 {
		t.Error("向量化失败后正文切片应已落库")
	}
}

// TestIngestDimMismatch 维度不匹配应中止该批（写入会导致检索失真）。
func TestIngestDimMismatch(t *testing.T) {
	f := newFixture(t)
	f.llm.embedDim = 512 // 与 KB.EmbeddingDim=1024 不一致
	rec := f.seedDoc(t, "维度不匹配时不能把错误维度的向量写入集合。")

	res, err := f.pipe.Ingest(context.Background(), f.kb, rec, nil)
	if err != nil {
		t.Fatalf("维度不匹配应按降级处理而非硬失败: %v", err)
	}
	if !res.Degraded {
		t.Error("维度不匹配必须标记 degraded")
	}
	if !strings.Contains(res.DegradeReason, "embedding_failed") {
		t.Errorf("降级原因应含 embedding_failed: %q", res.DegradeReason)
	}
}

// ---------- 补偿 ----------

// TestCompensate 修复向量化失败的切片。
func TestCompensate(t *testing.T) {
	f := newFixture(t)
	f.llm.embedErr = errors.New("boom")
	rec := f.seedDoc(t, "首次向量化失败的文档。补偿任务应当把它修好。正文内容足够长以便切出多个切片。")

	if _, err := f.pipe.Ingest(context.Background(), f.kb, rec, nil); err != nil {
		t.Fatalf("Ingest 失败: %v", err)
	}
	pending, err := f.ms.ListPendingChunks(context.Background(), 100)
	if err != nil || len(pending) == 0 {
		t.Fatalf("应有待补偿切片, err=%v n=%d", err, len(pending))
	}

	// 下游恢复后跑补偿
	f.llm.embedErr = nil
	fixed, err := f.pipe.Compensate(context.Background(), 100)
	if err != nil {
		t.Fatalf("Compensate 失败: %v", err)
	}
	if fixed <= 0 {
		t.Errorf("应修复切片, 实际 %d", fixed)
	}
	pending2, _ := f.ms.ListPendingChunks(context.Background(), 100)
	if len(pending2) != 0 {
		t.Errorf("补偿后不应再有待补偿切片, 实际 %d", len(pending2))
	}
}

// TestCompensateNoPending 无待补偿切片时直接返回 0。
func TestCompensateNoPending(t *testing.T) {
	f := newFixture(t)
	rec := f.seedDoc(t, "一切正常，没有待补偿。")
	if _, err := f.pipe.Ingest(context.Background(), f.kb, rec, nil); err != nil {
		t.Fatalf("Ingest 失败: %v", err)
	}
	fixed, err := f.pipe.Compensate(context.Background(), 100)
	if err != nil {
		t.Fatalf("Compensate 失败: %v", err)
	}
	if fixed != 0 {
		t.Errorf("无待补偿时 fixed 应为 0, 实际 %d", fixed)
	}
}

// TestCollectionOf 集合名约定。
func TestCollectionOf(t *testing.T) {
	f := newFixture(t)
	if got := f.pipe.collectionOf(2012); got != "aegis_kb_2012" {
		t.Errorf("集合名不符: %q", got)
	}
}

// TestVectorizeEmptyChunks rec.Chunks 为空必须显式报错（防止静默零命中）。
func TestVectorizeEmptyChunks(t *testing.T) {
	f := newFixture(t)
	rec := f.seedDoc(t, "正文")
	// 不回填 rec.Chunks，模拟上游漏接
	err := f.pipe.vectorize(context.Background(), f.kb, rec, nil, &Result{})
	if err == nil || !strings.Contains(err.Error(), "无切片可向量化") {
		t.Fatalf("空切片应显式报错, 实际 %v", err)
	}
}

// TestStartCompensatorDisabled interval<=0 时不启动 goroutine（不 panic）。
func TestStartCompensatorDisabled(t *testing.T) {
	f := newFixture(t)
	f.pipe.StartCompensator(context.Background(), 0) // 直接返回
	_ = vectordb.ErrUnavailable                      // 引用保持 import
}
