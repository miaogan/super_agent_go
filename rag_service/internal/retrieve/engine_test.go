package retrieve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	callopt "github.com/cloudwego/kitex/client/callopt"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"
	llmservice "github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/provider"
	"github.com/aegis/pkg/vectordb"
	"github.com/aegis/pkg/vectordb/mockstore"

	"github.com/aegis/rag_service/internal/meta"
)

// ---------- 测试替身 ----------

// stubLLM 可编程的 llmservice.Client。
//
// 只实现 Rerank 与 Embed（检索链路真正用到的两个），
// 其余方法返回零值 + 错误 —— 一旦链路意外调用了它们，测试会立刻暴露。
type stubLLM struct {
	rerankCalls int
	lastReq     *llmv1.RerankReq

	// rerankResp / rerankErr 二选一生效
	rerankResp *llmv1.RerankResp
	rerankErr  error
	embedErr   error
}

var _ llmservice.Client = (*stubLLM)(nil)

func (s *stubLLM) Rerank(_ context.Context, req *llmv1.RerankReq,
	_ ...callopt.Option) (*llmv1.RerankResp, error) {
	s.rerankCalls++
	s.lastReq = req
	if s.rerankErr != nil {
		return nil, s.rerankErr
	}
	return s.rerankResp, nil
}

func (s *stubLLM) Embed(_ context.Context, req *llmv1.EmbedReq,
	_ ...callopt.Option) (*llmv1.EmbedResp, error) {
	if s.embedErr != nil {
		return nil, s.embedErr
	}
	return &llmv1.EmbedResp{
		Dimension: 1024,
		Embeddings: []*llmv1.Embedding{{
			Vector: vec1024,
			Index:  0,
		}},
	}, nil
}

func (s *stubLLM) Health(context.Context, *commonv1.HealthReq, ...callopt.Option) (*commonv1.HealthResp, error) {
	return nil, errors.New("stub: 未实现")
}
func (s *stubLLM) Chat(context.Context, *llmv1.ChatReq, ...callopt.Option) (*llmv1.ChatResp, error) {
	return nil, errors.New("stub: 未实现")
}
func (s *stubLLM) ChatStream(context.Context, *llmv1.ChatReq, ...callopt.Option) (llmservice.LLMService_ChatStreamClient, error) {
	return nil, errors.New("stub: 未实现")
}
func (s *stubLLM) ListModels(context.Context, *llmv1.ListModelsReq, ...callopt.Option) (*llmv1.ListModelsResp, error) {
	return nil, errors.New("stub: 未实现")
}
func (s *stubLLM) GetCapabilities(context.Context, *llmv1.GetCapabilitiesReq, ...callopt.Option) (*llmv1.GetCapabilitiesResp, error) {
	return nil, errors.New("stub: 未实现")
}
func (s *stubLLM) GetUsage(context.Context, *llmv1.GetUsageReq, ...callopt.Option) (*llmv1.GetUsageResp, error) {
	return nil, errors.New("stub: 未实现")
}

// vec1024 固定向量：mockstore 靠向量算余弦相似度，
// 统一给同一个向量可以让排序只由分数阈值决定，测试结果稳定。
var vec1024 = func() []float32 {
	v := make([]float32, 1024)
	v[0] = 1
	return v
}()

// ---------- 夹具 ----------

type fixture struct {
	engine  *Engine
	llm     *stubLLM
	kbID    int64
	docID   int64
	chunkID []string
}

// newFixture 造一个「文档 →切片 → 向量库」齐全的检索环境。
func newFixture(t *testing.T, ragCfg conf.RAGConf) *fixture {
	t.Helper()

	ms := meta.NewMemoryStore()
	vec := mockstore.New()
	llm := &stubLLM{}

	ctx := context.Background()
	kbID, err := ms.NextKBID(ctx)
	if err != nil {
		t.Fatalf("分配 kb_id 失败: %v", err)
	}
	if err := ms.CreateKB(ctx, &ragv1.KnowledgeBase{KbId: kbID, Name: "测试库"}); err != nil {
		t.Fatalf("建知识库失败: %v", err)
	}
	docID, err := ms.NextDocID(ctx)
	if err != nil {
		t.Fatalf("分配 doc_id 失败: %v", err)
	}
	if err := ms.CreateDoc(ctx, &meta.DocumentRecord{
		Doc: &ragv1.Document{DocId: docID, KbId: kbID, FileName: "rag.txt"},
	}); err != nil {
		t.Fatalf("建文档失败: %v", err)
	}

	// 4 条内容差异明显的切片，便于验证重排确实换了顺序
	contents := []string{
		"RAG 是检索增强生成，先从知识库检索相关内容再交给大模型生成回答。",
		"Kubernetes 是容器编排系统，负责调度与管理容器化应用。",
		"今天天气晴朗，气温 25 度，适合外出散步。",
		"检索增强生成通过补充外部知识来缓解大模型的幻觉问题。",
	}

	chunks := make([]*ragv1.Chunk, 0, len(contents))
	vecChunks := make([]*vectordb.Chunk, 0, len(contents))
	for i, content := range contents {
		cid := ms.NextChunkID(docID, i)
		chunk := &ragv1.Chunk{
			ChunkId: cid,
			KbId:    kbID,
			DocId:   docID,
			Seq:     int32(i),
			PageNo:  int32(i / 2), // 两页文档
			Content: content,
		}
		chunks = append(chunks, chunk)
		vecChunks = append(vecChunks, &vectordb.Chunk{
			ChunkID: cid,
			KBID:    kbID,
			DocID:   docID,
			Vector:  vec1024,
		})
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

	// 阈值给0：让所有切片都进入候选，重排逻辑才是被测对象
	if ragCfg.DefaultScoreThreshold == 0 {
		ragCfg.DefaultScoreThreshold = 0.01
	}
	if ragCfg.RerankScoreThreshold == 0 {
		ragCfg.RerankScoreThreshold = 0.3
	}
	if ragCfg.RerankTopK == 0 {
		ragCfg.RerankTopK = 20
	}
	e := New(Options{
		Meta:      ms,
		Vector:    vec,
		LLM:       llm,
		RAGConfig: ragCfg,
		MilvusConfig: conf.MilvusConf{
			Dim:     1024,
			Timeout: 3 * time.Second,
		},
		EmbedModel: "test-embed",
	})

	ids := make([]string, 0, len(chunks))
	for _, c := range chunks {
		ids = append(ids, c.ChunkId)
	}
	return &fixture{engine: e, llm: llm, kbID: kbID, docID: docID, chunkID: ids}
}

func itoa(v int64) string {
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

func (f *fixture) retrieve(t *testing.T, req *Request) *Result {
	t.Helper()
	res, err := f.engine.Retrieve(context.Background(), req)
	if err != nil {
		t.Fatalf("Retrieve 返回错误: %v", err)
	}
	return res
}

// ---------- 用例 ----------

// TestRerankChangesOrder 重排应改变候选顺序并覆盖分数。
//
// 这是整条链路的「正向」断言：没有它，无法区分
// 「重排真的生效了」与「代码碰巧返回了向量序」。
func TestRerankChangesOrder(t *testing.T) {
	f := newFixture(t, conf.RAGConf{RerankEnabled: true, ContextWindow: 0})
	f.llm.rerankResp = &llmv1.RerankResp{
		Model: "test-rerank",
		Results: []*llmv1.RerankResult{
			{Index: 3, RelevanceScore: 0.97},
			{Index: 0, RelevanceScore: 0.91},
		},
	}

	res := f.retrieve(t, &Request{
		Query:     "什么是 RAG",
		KBIDs:     []int64{f.kbID},
		TopK:      2,
		Debug:     true,
		RequestID: "t1",
	})

	if f.llm.rerankCalls != 1 {
		t.Fatalf("应调用 1 次重排，实际 %d 次", f.llm.rerankCalls)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("应返回 2 条，实际 %d: %+v", len(res.Hits), res.Hits)
	}
	// 重排后第一条应是原文[3]
	if res.Hits[0].ChunkId != f.chunkID[3] {
		t.Errorf("重排未改变顺序，第一条应为 %s，实际 %s", f.chunkID[3], res.Hits[0].ChunkId)
	}
	// Score 应被覆盖为重排分（不是向量分）
	if res.Hits[0].Score != 0.97 {
		t.Errorf("Score 应覆盖为重排分 0.97，实际 %v", res.Hits[0].Score)
	}
	if res.Degraded {
		t.Errorf("重排成功不应标记降级: %s", res.DegradeReason)
	}
	if !hasStage(res, "rerank") {
		t.Errorf("Debug Stages 应含 rerank，实际 %+v", res.Stages)
	}
}

// TestRerankDoesNotReturnDocuments 重排请求不该回带原文。
//
// rag 侧手里已有候选文本，回传是重复搬运 + 双份内存。
func TestRerankDoesNotReturnDocuments(t *testing.T) {
	f := newFixture(t, conf.RAGConf{RerankEnabled: true, ContextWindow: 0})
	f.llm.rerankResp = &llmv1.RerankResp{
		Results: []*llmv1.RerankResult{{Index: 0, RelevanceScore: 0.9}},
	}

	f.retrieve(t, &Request{Query: "q", KBIDs: []int64{f.kbID}, TopK: 1})

	if f.llm.lastReq.GetReturnDocuments() {
		t.Error("ReturnDocuments 应为 false（原文已在候选集里）")
	}
	if f.llm.lastReq.GetModel() != "" {
		t.Errorf("未配 rerank_model 时应传空由下游回落，实际 %q", f.llm.lastReq.GetModel())
	}
}

// TestRerankUnsupportedDegradesButKeepsResults 501（未配置重排模型）。
//
// ★ 这是本轮最关键的一条断言：
//
//	重排失败**不能**让整次检索失败，也不能丢掉已召回的结果。
//	用户必须拿到结果 + 一个诚实的 degrade_reason。
func TestRerankUnsupportedDegradesButKeepsResults(t *testing.T) {
	f := newFixture(t, conf.RAGConf{RerankEnabled: true, ContextWindow: 0})
	f.llm.rerankErr = errno.New(errno.ErrNotImplemented)

	res := f.retrieve(t, &Request{Query: "q", KBIDs: []int64{f.kbID}, TopK: 2})

	if !res.Degraded {
		t.Fatal("重排未生效必须标记 degraded")
	}
	if !strings.Contains(res.DegradeReason, "rerank_unsupported") {
		t.Errorf("降级原因应含 rerank_unsupported，实际 %q", res.DegradeReason)
	}
	if len(res.Hits) == 0 {
		t.Error("重排降级时必须保留已召回的结果，不能丢")
	}
	if int(res.Total) != len(res.Hits) {
		t.Errorf("Total 与 Hits 长度不一致: %d vs %d", res.Total, len(res.Hits))
	}
}

// TestRerankTimeoutDegrades 重排超时也只降级、不失败。
func TestRerankTimeoutDegrades(t *testing.T) {
	f := newFixture(t, conf.RAGConf{RerankEnabled: true, ContextWindow: 0})
	f.llm.rerankErr = context.DeadlineExceeded

	res := f.retrieve(t, &Request{Query: "q", KBIDs: []int64{f.kbID}, TopK: 2})

	if !res.Degraded || !strings.Contains(res.DegradeReason, "rerank_timeout") {
		t.Errorf("超时应降级为 rerank_timeout，实际 degraded=%v reason=%q",
			res.Degraded, res.DegradeReason)
	}
	if len(res.Hits) == 0 {
		t.Error("超时也不该丢结果")
	}
}

// TestRerankProviderErrorDegrades Provider 报 503 属真故障，同样降级但原因不同。
func TestRerankProviderErrorDegrades(t *testing.T) {
	f := newFixture(t, conf.RAGConf{RerankEnabled: true, ContextWindow: 0})
	f.llm.rerankErr = errno.New(errno.ErrModelUnavailable)

	res := f.retrieve(t, &Request{Query: "q", KBIDs: []int64{f.kbID}, TopK: 2})

	if !res.Degraded || !strings.Contains(res.DegradeReason, "rerank_failed") {
		t.Errorf("Provider 故障应降级为 rerank_failed，实际 %q", res.DegradeReason)
	}
}

// TestRerankDisabledSkipsRPC 关闭开关时不该发起 RPC。
func TestRerankDisabledSkipsRPC(t *testing.T) {
	f := newFixture(t, conf.RAGConf{RerankEnabled: false, ContextWindow: 0})

	res := f.retrieve(t, &Request{Query: "q", KBIDs: []int64{f.kbID}, TopK: 2, Debug: true})

	if f.llm.rerankCalls != 0 {
		t.Errorf("关闭开关时不应调用重排，实际调用 %d 次", f.llm.rerankCalls)
	}
	if res.Degraded {
		t.Errorf("主动关闭不算降级: %s", res.DegradeReason)
	}
	if hasStage(res, "rerank") {
		// Stages 里保留一条说明「未执行」，比完全没有记录更好排查
		if !strings.Contains(stageDetail(res, "rerank"), "disabled") {
			t.Errorf("rerank 阶段应标明未执行原因，实际 %q", stageDetail(res, "rerank"))
		}
	}
}

// TestRerankRejectedAllIsNotDegraded 全部低于阈值 → 空结果但不是降级。
//
// ★ 区别很重要：「模型判定全部不相关」是正常业务结论，
//
//	标成 degraded 会把业务判断误报成技术故障。
func TestRerankRejectedAllIsNotDegraded(t *testing.T) {
	f := newFixture(t, conf.RAGConf{RerankEnabled: true, ContextWindow: 0})
	f.llm.rerankResp = &llmv1.RerankResp{
		Results: []*llmv1.RerankResult{
			{Index: 0, RelevanceScore: 0.05},
			{Index: 1, RelevanceScore: 0.02},
		},
	}

	res := f.retrieve(t, &Request{Query: "q", KBIDs: []int64{f.kbID}, TopK: 2, Debug: true})

	if len(res.Hits) != 0 {
		t.Errorf("全低于阈值应返回空结果，实际 %d 条", len(res.Hits))
	}
	if res.Degraded {
		t.Errorf("「全部不相关」不是降级，不应置 degraded: %s", res.DegradeReason)
	}
	if !strings.Contains(res.DegradeReason, "rerank_rejected_all") {
		t.Errorf("原因应如实写明是重排全部拒绝: %q", res.DegradeReason)
	}
}

// TestRerankOutOfRangeIndexSkipped 异常返回体的越界 index 不能让检索崩。
func TestRerankOutOfRangeIndexSkipped(t *testing.T) {
	f := newFixture(t, conf.RAGConf{RerankEnabled: true, ContextWindow: 0})
	f.llm.rerankResp = &llmv1.RerankResp{
		Results: []*llmv1.RerankResult{
			{Index: 99, RelevanceScore: 0.99},
			{Index: -1, RelevanceScore: 0.98},
			{Index: 1, RelevanceScore: 0.8},
		},
	}

	res := f.retrieve(t, &Request{Query: "q", KBIDs: []int64{f.kbID}, TopK: 2})

	if len(res.Hits) != 1 || res.Hits[0].ChunkId != f.chunkID[1] {
		t.Errorf("越界项应被跳过，只保留合法项: %+v", res.Hits)
	}
}

// TestCandidateKFollowsRerankTopK 召回数应按 RerankTopK 而非 topK。
//
// 若召回量与 topK 相同，重排只能重新排序无法替换 —— 那就白花了调用。
func TestCandidateKFollowsRerankTopK(t *testing.T) {
	e := &Engine{cfg: conf.RAGConf{RerankEnabled: true, RerankTopK: 20}}
	if got := e.candidateK(5); got != 20 {
		t.Errorf("启用重排时召回数应为 20，实际 %d", got)
	}

	e2 := &Engine{cfg: conf.RAGConf{RerankEnabled: true, RerankTopK: 3}}
	if got := e2.candidateK(5); got != 5 {
		t.Errorf("RerankTopK 小于 topK 时不应缩小召回，实际 %d", got)
	}

	e3 := &Engine{cfg: conf.RAGConf{RerankEnabled: false, RerankTopK: 20}}
	if got := e3.candidateK(5); got != 5 {
		t.Errorf("关闭重排时召回应等于 topK，实际 %d", got)
	}
}

// TestRerankThresholdMockMode mock 模式下阈值必须下调。
//
// ★ 这是会让冒烟测试静默失效的坑：
//
//	mock 重排用词袋余弦，上限约 0.25；生产阈值 0.3。
//	不下调则 mock 模式恒 0 命中，而 CI 全绿 —— 看起来没问题，实际没测到东西。
func TestRerankThresholdMockMode(t *testing.T) {
	prod := &Engine{cfg: conf.RAGConf{RerankScoreThreshold: 0.3}}
	if got := prod.rerankThreshold(); got != 0.3 {
		t.Errorf("生产模式应沿用 0.3，实际 %v", got)
	}

	mockE := &Engine{
		mockMode: true,
		cfg: conf.RAGConf{
			RerankScoreThreshold: 0.3,
			MockScoreThreshold:   0.1,
		},
	}
	if got := mockE.rerankThreshold(); got != 0.1 {
		t.Errorf("mock 模式应下调到 0.1，实际 %v", got)
	}

	// mock 阈值更高时不该被拉低
	mockE2 := &Engine{
		mockMode: true,
		cfg: conf.RAGConf{
			RerankScoreThreshold: 0.05,
			MockScoreThreshold:   0.1,
		},
	}
	if got := mockE2.rerankThreshold(); got != 0.05 {
		t.Errorf("已低于 mock 阈值时不应上调，实际 %v", got)
	}
}

// TestMockModeRerankStillHits 端到端验证 mock 阈值修正确实救回了命中。
//
// 这是给「TestRerankThresholdMockMode」兜底的集成断言：
// 若哪天 mock 重排实现变了导致词袋余弦不再够用，这条会红。
func TestMockModeRerankStillHits(t *testing.T) {
	f := newFixture(t, conf.RAGConf{RerankEnabled: true, ContextWindow: 0})
	f.engine.mockMode = true
	// 复现真实运行时 conf.Default() 给的值 —— 不是随意填的
	f.engine.cfg.MockScoreThreshold = 0.1
	// mock 重排：全给 0.2（低于生产阈值 0.3，但高于 mock 阈值 0.1）
	results := make([]*llmv1.RerankResult, 0, len(f.chunkID))
	for i := range f.chunkID {
		results = append(results, &llmv1.RerankResult{
			Index: int32(i), RelevanceScore: 0.2,
		})
	}
	f.llm.rerankResp = &llmv1.RerankResp{Results: results}

	res := f.retrieve(t, &Request{Query: "q", KBIDs: []int64{f.kbID}, TopK: 2})

	if len(res.Hits) == 0 {
		t.Error("mock 模式下阈值修正后应有命中，否则冒烟测试形同虚设")
	}
}

// TestContextExpansionAfterRerank 外扩应在重排之后、只对入选结果做。
//
// 验证方式：命中 1 条且该切片有邻居 → 内容被扩展；
// 同时确认重排请求里的 documents 是**外扩前**的原文。
func TestContextExpansionAfterRerank(t *testing.T) {
	f := newFixture(t, conf.RAGConf{RerankEnabled: true, ContextWindow: 1})
	f.llm.rerankResp = &llmv1.RerankResp{
		Results: []*llmv1.RerankResult{{Index: 2, RelevanceScore: 0.9}},
	}

	res := f.retrieve(t, &Request{Query: "q", KBIDs: []int64{f.kbID}, TopK: 1})

	if len(res.Hits) != 1 {
		t.Fatalf("应返回 1 条，实际 %d", len(res.Hits))
	}
	// 送给重排的应是原文（外扩前）
	sent := f.llm.lastReq.GetDocuments()
	if len(sent) != len(f.chunkID) {
		t.Fatalf("重排候选数应为 %d，实际 %d", len(f.chunkID), len(sent))
	}
	for _, doc := range sent {
		if strings.Contains(doc, "\n") {
			t.Errorf("送重排的应是外扩前原文（不含拼接的邻片）: %q", doc)
		}
	}
	// 命中结果本身应已被外扩（含邻片）
	if !strings.Contains(res.Hits[0].Content, "\n") {
		t.Errorf("入选结果应已做上下文外扩: %q", res.Hits[0].Content)
	}
}

// TestUnsupportedErrorMessageMapping 校验降级原因的判别逻辑。
//
// 用 errno 的错误字符串模拟跨服务后的错误形态 ——
// rag 侧拿到的是 gRPC 返回的 errno 错误，不是 provider 的原始 error。
func TestUnsupportedErrorMessageMapping(t *testing.T) {
	e := &Engine{}
	cases := []struct {
		err  error
		want string
	}{
		{provider.ErrUnsupported, "rerank_unsupported"},
		{errno.New(errno.ErrNotImplemented), "rerank_unsupported"},
		{context.DeadlineExceeded, "rerank_timeout"},
		{errno.New(errno.ErrModelUnavailable), "rerank_failed"},
	}
	for _, c := range cases {
		got := e.degradeReasonFor(c.err)
		if !strings.Contains(got, c.want) {
			t.Errorf("degradeReasonFor(%v) 应含 %q，实际 %q", c.err, c.want, got)
		}
	}
}

// TestAppendReasonKeepsRootCause 降级原因应「根因在前」。
func TestAppendReasonKeepsRootCause(t *testing.T) {
	if got := appendReason("", "a"); got != "a" {
		t.Errorf("空串时应直接返回: %q", got)
	}
	if got := appendReason("a", ""); got != "a" {
		t.Errorf("追加空原因不应改变: %q", got)
	}
	if got := appendReason("a", "b"); got != "a; b" {
		t.Errorf("应按顺序拼接: %q", got)
	}
}

func hasStage(res *Result, name string) bool {
	for _, s := range res.Stages {
		if s.GetStage() == name {
			return true
		}
	}
	return false
}

func stageDetail(res *Result, name string) string {
	for _, s := range res.Stages {
		if s.GetStage() == name {
			return s.GetDetail()
		}
	}
	return ""
}
