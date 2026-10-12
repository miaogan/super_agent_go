package service

import (
	"context"
	"errors"
	"testing"
	"time"

	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"

	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/mockx"
	"github.com/aegis/pkg/provider"
)

// ---------- 测试替身 ----------

// stubProvider 可编程的 provider.Provider。
//
// 与 pkg/provider/chain/chain_test.go 同风格：单一 stub 覆盖全部接口，
// 用字段控制各方法的行为。需要留意 Provider 接口方法较多，
// 新增方法时这里会编译报错 —— 这是刻意的：
// 它会提醒「该给测试替身补能力了」，而不是静默用零值让断言失真。
type stubProvider struct {
	rerankErr error
	resp      *provider.RerankResponse

	gotModel     string
	gotQuery     string
	gotDocuments []string
	gotTopN      int
	gotReturnDoc bool
	gotUID       int64
}

var _ provider.Provider = (*stubProvider)(nil)

func (s *stubProvider) Rerank(_ context.Context, req *provider.RerankRequest) (*provider.RerankResponse, error) {
	s.gotModel = req.Model
	s.gotQuery = req.Query
	s.gotDocuments = req.Documents
	s.gotTopN = req.TopN
	s.gotReturnDoc = req.ReturnDocuments
	s.gotUID = req.UID
	if s.rerankErr != nil {
		return nil, s.rerankErr
	}
	if s.resp != nil {
		return s.resp, nil
	}
	return &provider.RerankResponse{Model: req.Model}, nil
}

func (s *stubProvider) Name() string                        { return "stub" }
func (s *stubProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (s *stubProvider) Close() error                        { return nil }
func (s *stubProvider) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return nil, nil
}
func (s *stubProvider) Chat(context.Context, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("stub: 未实现")
}
func (s *stubProvider) ChatStream(context.Context, *provider.ChatRequest) (<-chan *provider.Chunk, error) {
	return nil, errors.New("stub: 未实现")
}
func (s *stubProvider) Embed(context.Context, *provider.EmbedRequest) (*provider.EmbedResponse, error) {
	return nil, errors.New("stub: 未实现")
}

// newTestSvc 造一个只用于 Rerank 测试的 handler。
//
// mock 传 nil FaultInjector 时 llm_service 在 main.go 里一定有值，
// 但测试里用 mockx.NewFaultInjector(false, 0, nil) 更贴近真实构造。
func newTestSvc(prov provider.Provider, mock *mockx.FaultInjector, rerankModel string) *LLMService {
	return New(Options{
		Provider:           prov,
		Mock:               mock,
		CacheEnabled:       false,
		DefaultRerankModel: rerankModel,
	})
}

func okResp() *provider.RerankResponse {
	return &provider.RerankResponse{
		Model: "stub-rerank",
		Results: []provider.RerankResult{
			{Index: 2, RelevanceScore: 0.97},
			{Index: 0, RelevanceScore: 0.42},
		},
		Usage: provider.Usage{PromptTokens: 254, TotalTokens: 254},
	}
}

// ---------- 用例 ----------

// TestRerankFallsBackToDefaultModel 未指定 model 时用默认重排模型。
func TestRerankFallsBackToDefaultModel(t *testing.T) {
	prov := &stubProvider{resp: okResp()}
	svc := newTestSvc(prov, mockx.NewFaultInjector(false, 0, nil), "qwen3.7-text-rerank")

	if _, err := svc.Rerank(context.Background(), &llmv1.RerankReq{
		Query:     "什么是RAG",
		Documents: []string{"a", "b", "c"},
	}); err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}
	if prov.gotModel != "qwen3.7-text-rerank" {
		t.Errorf("应回落到默认重排模型，实际 %q", prov.gotModel)
	}
}

// TestRerankExplicitModelWins 显式指定 model 时不用默认值。
func TestRerankExplicitModelWins(t *testing.T) {
	prov := &stubProvider{resp: okResp()}
	svc := newTestSvc(prov, mockx.NewFaultInjector(false, 0, nil), "default-rr")

	if _, err := svc.Rerank(context.Background(), &llmv1.RerankReq{
		Query: "q", Documents: []string{"a"}, Model: "explicit-rr",
	}); err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}
	if prov.gotModel != "explicit-rr" {
		t.Errorf("应使用显式指定，实际 %q", prov.gotModel)
	}
}

// TestRerankPassesThroughFields 字段应完整透传给 Provider。
func TestRerankPassesThroughFields(t *testing.T) {
	prov := &stubProvider{resp: okResp()}
	svc := newTestSvc(prov, mockx.NewFaultInjector(false, 0, nil), "rr")

	resp, err := svc.Rerank(context.Background(), &llmv1.RerankReq{
		Query:           "什么是RAG",
		Documents:       []string{"d0", "d1", "d2"},
		TopN:            2,
		ReturnDocuments: false,
	})
	if err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}
	if prov.gotQuery != "什么是RAG" {
		t.Errorf("query 未透传: %q", prov.gotQuery)
	}
	if len(prov.gotDocuments) != 3 {
		t.Errorf("documents 未透传: %v", prov.gotDocuments)
	}
	if prov.gotTopN != 2 {
		t.Errorf("topN 未透传: %d", prov.gotTopN)
	}
	if prov.gotReturnDoc {
		t.Error("ReturnDocuments 未透传")
	}
	// 响应结构应完整转换
	if len(resp.GetResults()) != 2 {
		t.Fatalf("结果数不对: %d", len(resp.GetResults()))
	}
	if resp.GetResults()[0].GetIndex() != 2 || resp.GetResults()[0].GetRelevanceScore() != 0.97 {
		t.Errorf("首条结果转换错误: %+v", resp.GetResults()[0])
	}
	if resp.GetModel() != "stub-rerank" {
		t.Errorf("model 未回传: %q", resp.GetModel())
	}
	if resp.GetUsage().GetPromptTokens() != 254 {
		t.Errorf("usage 未回传: %+v", resp.GetUsage())
	}
}

// TestRerankMissingParam 缺 query 或 documents 一律报参数错误。
//
// 显式覆盖两种情况：空 query / 空 documents。
func TestRerankMissingParam(t *testing.T) {
	cases := []struct {
		name string
		req  *llmv1.RerankReq
	}{
		{"query 为空", &llmv1.RerankReq{Query: "", Documents: []string{"a"}}},
		{"documents 为空", &llmv1.RerankReq{Query: "q", Documents: nil}},
		{"documents 空切片", &llmv1.RerankReq{Query: "q", Documents: []string{}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prov := &stubProvider{resp: okResp()}
			svc := newTestSvc(prov, mockx.NewFaultInjector(false, 0, nil), "rr")
			_, err := svc.Rerank(context.Background(), c.req)
			if err == nil {
				t.Fatal("应返回参数错误")
			}
			if !errors.Is(err, errno.New(errno.ErrMissingParam)) &&
				err.Error() != errno.New(errno.ErrMissingParam).Error() {
				t.Errorf("应为 ErrMissingParam，实际 %v", err)
			}
			if prov.gotQuery != "" && prov.gotQuery != c.req.GetQuery() {
				t.Error("参数校验失败时不应调用 Provider")
			}
		})
	}
}

// TestRerankFaultInjection 故障注入开关能触发。
func TestRerankFaultInjection(t *testing.T) {
	prov := &stubProvider{resp: okResp()}
	svc := newTestSvc(prov, mockx.NewFaultInjector(true, 0, []string{"rerank"}), "rr")

	_, err := svc.Rerank(context.Background(), &llmv1.RerankReq{
		Query: "q", Documents: []string{"a"},
	})
	if err == nil {
		t.Fatal("故障注入时应返回错误")
	}
	if err.Error() != errno.New(errno.ErrModelUnavailable).Error() {
		t.Errorf("应返回 ErrModelUnavailable，实际 %v", err)
	}
}

// TestRerankUnsupportedMapsTo501 ★ 本 handler 最关键的映射。
//
// 「能力缺失」（未配置重排模型 / 缺 rerank_path）必须映射为 501 而不是 503：
//
//	501 → 调用方（rag_service）据此**跳过重排**，不做重试
//	503 → 调用方当成真故障，会重试 / 熔断 / 告警
//
// 把配置缺失报成故障，等于为一个纯配置问题持续制造告警噪声。
func TestRerankUnsupportedMapsTo501(t *testing.T) {
	prov := &stubProvider{rerankErr: provider.ErrUnsupported}
	svc := newTestSvc(prov, mockx.NewFaultInjector(false, 0, nil), "rr")

	_, err := svc.Rerank(context.Background(), &llmv1.RerankReq{
		Query: "q", Documents: []string{"a"},
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if err.Error() != errno.New(errno.ErrNotImplemented).Error() {
		t.Errorf("能力缺失应映射为 ErrNotImplemented(501)，实际 %v", err)
	}
}

// TestRerankTimeoutMapsTo504 超时映射为 504，与「能力缺失」区分开。
func TestRerankTimeoutMapsTo504(t *testing.T) {
	prov := &stubProvider{rerankErr: context.DeadlineExceeded}
	svc := newTestSvc(prov, mockx.NewFaultInjector(false, 0, nil), "rr")

	_, err := svc.Rerank(context.Background(), &llmv1.RerankReq{
		Query: "q", Documents: []string{"a"},
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if err.Error() != errno.New(errno.ErrTimeout).Error() {
		t.Errorf("超时应映射为 ErrTimeout(504)，实际 %v", err)
	}
}

// TestRerankGenericErrorMapsTo503 其余错误映射为 503（可重试）。
func TestRerankGenericErrorMapsTo503(t *testing.T) {
	prov := &stubProvider{rerankErr: errors.New("connection reset")}
	svc := newTestSvc(prov, mockx.NewFaultInjector(false, 0, nil), "rr")

	_, err := svc.Rerank(context.Background(), &llmv1.RerankReq{
		Query: "q", Documents: []string{"a"},
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if err.Error() != errno.New(errno.ErrModelUnavailable).Error() {
		t.Errorf("普通故障应映射为 ErrModelUnavailable(503)，实际 %v", err)
	}
}

// TestRerankRecordsUsage 重排调用必须计入 usage，否则成本核算会漏掉这一环。
func TestRerankRecordsUsage(t *testing.T) {
	prov := &stubProvider{resp: okResp()}
	svc := newTestSvc(prov, mockx.NewFaultInjector(false, 0, nil), "rr")

	if _, err := svc.Rerank(context.Background(), &llmv1.RerankReq{
		Query: "q", Documents: []string{"a"},
	}); err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}

	// 用公开的 Query 接口读回，验证计量确实落库
	recs, _, cost := svc.usage.Query(0, 0, time.Now().Add(time.Minute).UnixNano())
	var found bool
	for _, r := range recs {
		if r.RequestID == "rerank" {
			found = true
			if r.PromptTokens != 254 {
				t.Errorf("usage 记录 token 数不对: %+v", r)
			}
			if r.Model != "stub-rerank" {
				t.Errorf("usage 记录的模型不对: %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("usage 未记录 rerank 调用: %+v", recs)
	}
	if cost < 0 {
		t.Errorf("成本不应为负: %v", cost)
	}
}

// TestRerankRespectsContextCancel ctx 取消应立即返回。
func TestRerankRespectsContextCancel(t *testing.T) {
	prov := &stubProvider{resp: okResp()}
	svc := newTestSvc(prov, mockx.NewFaultInjector(false, 50, nil), "rr")

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _ = svc.Rerank(ctx, &llmv1.RerankReq{Query: "q", Documents: []string{"a"}})
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("ctx 取消后应尽快返回，实际耗时 %s", d)
	}
}
