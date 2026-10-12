// Package service 实现 llm_service 的业务 handler。
//
// 定位：全系统唯一的模型调用出口。
//
//	agent_service ─┐
//	rag_service   ─┼─→ llm_service ─→ Provider（eino / rawhttp / mock）
//	gateway       ─┘
//
// 因此本服务承担三件别处不该做的事：
//  1. 计量（token 归集与成本核算）
//  2. 熔断降级（Provider 链）
//  3. Embedding 缓存（降低 RAG 成本）
package service

import (
	"context"
	"strings"
	"sync"
	"time"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"

	"github.com/aegis/pkg/cache"
	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/mockx"
	"github.com/aegis/pkg/provider"
)

// ToolResolver 把工具名列表解析成完整 Tool 定义。
//
// 为什么在 llm_service 里加这层：ChatReq.tools 是 repeated string（名字列表），
// 而 provider.ChatRequest.Tools 需要完整 JSON Schema 定义。
// agent_service 把名字传过来，llm_service 从 PG tools 表补全。
// （测试里可以用 mock 实现，避免整个 llm_service 依赖 DB）。
type ToolResolver interface {
	ResolveTools(ctx context.Context, names []string) ([]provider.Tool, error)
}

// LLMService 实现 llmv1.LLMService
type LLMService struct {
	prov provider.Provider
	mock *mockx.FaultInjector

	embedCache *cache.Cache
	usage      *UsageRecorder

	defaultModel       string
	defaultEmbedModel  string
	defaultRerankModel string
	defaultDim         int
	maxTokens          int

	// toolRes 可选工具解析器：
	//   nil → req.Tools 被忽略（兼容旧行为）
	//   非 nil → 把名字列表转完整 Tool 定义传给 Provider
	toolRes ToolResolver

	startedAt time.Time
}

// Options 构造参数
type Options struct {
	Provider          provider.Provider
	Mock              *mockx.FaultInjector
	CacheEnabled      bool
	CacheTTL          time.Duration
	DefaultModel      string
	DefaultEmbedModel string
	// DefaultRerankModel 默认重排模型（逻辑名）。
	//
	// ★ 取自 conf.RAGConf.RerankModel 而非 LLMConf ——
	//   重排是「检索链路的环节」，配置归 rag 段更贴合语义；
	//   这里只做一次转手，不在 LLMConf 里再开一个重复字段。
	DefaultRerankModel string
	DefaultEmbedDim    int
	MaxTokens          int

	// ToolResolver 可选工具解析器。nil 时忽略 req.Tools。
	ToolResolver ToolResolver
}

// New 构造 handler
func New(opt Options) *LLMService {
	var embedCache *cache.Cache
	if opt.CacheEnabled {
		embedCache = cache.New(cache.Options{TTL: opt.CacheTTL, MaxItems: 4096})
	}
	return &LLMService{
		prov:               opt.Provider,
		mock:               opt.Mock,
		embedCache:         embedCache,
		usage:              NewUsageRecorder(1000),
		defaultModel:       opt.DefaultModel,
		defaultEmbedModel:  opt.DefaultEmbedModel,
		defaultRerankModel: opt.DefaultRerankModel,
		defaultDim:         opt.DefaultEmbedDim,
		maxTokens:          opt.MaxTokens,
		toolRes:            opt.ToolResolver,
		startedAt:          time.Now(),
	}
}

var _ llmv1.LLMService = (*LLMService)(nil)

// ---------- 健康检查 ----------

func (s *LLMService) Health(ctx context.Context, _ *commonv1.HealthReq) (*commonv1.HealthResp, error) {
	// mock 模式下附带调用统计，便于冒烟脚本确认 Provider 真的被调用过
	if ms, ok := s.prov.(mockStats); ok {
		reqs, tokens := ms.Stats()
		logger.Debug(ctx, "llm mock stats", "requests", reqs, "tokens", tokens)
	}
	if st := s.embedCache.Stats(); st.Size > 0 {
		logger.Debug(ctx, "embed cache", "size", st.Size, "hit_rate", st.HitRate)
	}

	return &commonv1.HealthResp{
		Healthy:   !s.mock.ShouldFail("llm"),
		Service:   "llm_service",
		Version:   "dev",
		UptimeSec: uint64(time.Since(s.startedAt).Seconds()),
		MockMode:  s.prov.Name() == "mock",
	}, nil
}

// mockStats mock provider 的统计接口
type mockStats interface {
	Stats() (int64, int64)
}

// ---------- Chat ----------

func (s *LLMService) Chat(ctx context.Context, req *llmv1.ChatReq) (*llmv1.ChatResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	if len(req.GetMessages()) == 0 {
		return nil, errno.New(errno.ErrMissingParam)
	}

	pReq := s.toProviderRequest(req)
	if s.mock.ShouldFail("llm") {
		return nil, errno.New(errno.ErrModelUnavailable)
	}

	start := time.Now()
	resp, err := s.prov.Chat(ctx, pReq)
	if err != nil {
		logger.Warn(ctx, "chat failed", "model", req.GetModel(), "err", err)
		return nil, classifyProviderErr(err)
	}
	elapsed := time.Since(start)

	s.usage.Record(&UsageRecord{
		RequestID:        req.GetRequestId(),
		UID:              req.GetIdentity().GetUid(),
		Model:            resp.Model,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		Degraded:         resp.Degraded,
		LatencyMS:        elapsed.Milliseconds(),
	})

	out := &llmv1.ChatResp{
		Content:      resp.Content,
		Reasoning:    resp.Reasoning,
		Model:        resp.Model,
		Provider:     resp.Provider,
		FinishReason: resp.FinishReason,
		Usage:        toPBUsage(resp.Usage),
	}
	for _, tc := range resp.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, &llmv1.ToolCall{
			Id:        tc.ID,
			Name:      tc.Name,
			Arguments: tc.Arguments,
		})
	}
	if resp.Degraded {
		out.Degradation = &commonv1.Degradation{
			Degraded: true,
			Reason:   "provider_degraded",
			Detail:   resp.DegradeReason,
		}
	}
	return out, nil
}

// ChatStream 流式对话
func (s *LLMService) ChatStream(req *llmv1.ChatReq, stream llmv1.LLMService_ChatStreamServer) error {
	ctx := stream.Context()
	if err := s.mock.Sleep(ctx); err != nil {
		return err
	}
	if len(req.GetMessages()) == 0 {
		return errno.New(errno.ErrMissingParam)
	}
	if s.mock.ShouldFail("llm") {
		return errno.New(errno.ErrModelUnavailable)
	}

	ch, err := s.prov.ChatStream(ctx, s.toProviderRequest(req))
	if err != nil {
		return classifyProviderErr(err)
	}

	var usage provider.Usage
	var degraded bool
	var degradeReason string
	var sb strings.Builder

	for chunk := range ch {
		if chunk.Degraded {
			degraded = true
			degradeReason = chunk.DegradeReason
		}
		if chunk.Delta != "" {
			sb.WriteString(chunk.Delta)
		}
		if chunk.Done {
			usage = chunk.Usage
			// 末帧：带 usage 与降级信息
			return stream.Send(&llmv1.ChatChunk{
				Done:        true,
				Usage:       toPBUsage(usage),
				Degradation: degradationOf(degraded, degradeReason),
			})
		}
		// 中间帧：delta + 可能的 tool_call 增量。
		// ★ ToolCallDelta 必须透传：agent_service 的流式 ReAct 循环
		//   靠它在 consumeStream 里收集 tool_calls 并执行工具。
		//   rawhttp 侧已按 index 累积完整 arguments，此处拿到的是完整 ToolCall。
		out := &llmv1.ChatChunk{Delta: chunk.Delta}
		if tc := chunk.ToolCallDelta; tc != nil {
			out.ToolCallDelta = &llmv1.ToolCall{
				Id:        tc.ID,
				Name:      tc.Name,
				Arguments: tc.Arguments,
			}
		}
		if err := stream.Send(out); err != nil {
			return err
		}
	}

	// Provider 流意外结束（未发 Done 帧）：补一个末帧，
	// 否则前端会一直等在 "生成中" 状态
	return stream.Send(&llmv1.ChatChunk{
		Done:        true,
		Usage:       toPBUsage(usage),
		Degradation: degradationOf(degraded, degradeReason),
	})
}

// ---------- Embed ----------

func (s *LLMService) Embed(ctx context.Context, req *llmv1.EmbedReq) (*llmv1.EmbedResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	if len(req.GetTexts()) == 0 {
		return nil, errno.New(errno.ErrMissingParam)
	}

	model := req.GetModel()
	if model == "" {
		model = s.defaultEmbedModel
	}
	if s.mock.ShouldFail("embed") {
		return nil, errno.New(errno.ErrModelUnavailable)
	}

	// ★ enable_sparse 时跳过 dense 缓存：
	//   缓存只存稠密向量，稀疏向量无法从缓存补齐；
	//   若命中缓存只拿 dense、缺失 sparse，混合检索会静默少一路。
	//   代价是 enable_sparse 的请求总是全量调用 Provider —— 正确性优先。
	enableSparse := req.GetEnableSparse()

	// 缓存命中判定。
	// hitVecs[i] 是第 i 条的缓存向量（命中时非 nil），
	// pending 收集需要向量的下标。
	texts := req.GetTexts()
	hitVecs := make([][]float32, len(texts))
	cached := make([]bool, len(texts))
	pending := make([]int, 0, len(texts))
	for i, text := range texts {
		if enableSparse {
			pending = append(pending, i)
			continue
		}
		vec, ok := s.embedCache.Get(model, s.defaultDim, req.GetInputType(), text)
		if !ok {
			pending = append(pending, i)
			continue
		}
		// ★ 维度自愈：缓存里的向量长度与当前配置不一致时，
		//   宁可当作未命中也不能混用 —— 混合不同维度的向量会让
		//   Milvus 插入直接失败，或让检索结果彻底失真。
		if s.defaultDim > 0 && len(vec) != s.defaultDim {
			pending = append(pending, i)
			continue
		}
		cached[i] = true
		hitVecs[i] = vec
	}

	// 全部命中则完全不调用 Provider。
	// 注意 fill 必须非 nil：buildEmbedResp 会调用它。
	if len(pending) == 0 {
		return s.buildEmbedResp(model, req, cached, provider.Usage{}, func() [][]float32 {
			return hitVecs
		}, nil), nil
	}

	// 只向量化未命中的部分
	missTexts := make([]string, 0, len(pending))
	for _, idx := range pending {
		missTexts = append(missTexts, texts[idx])
	}

	pResp, err := s.prov.Embed(ctx, &provider.EmbedRequest{
		Model:        model,
		Texts:        missTexts,
		UID:          req.GetIdentity().GetUid(),
		Tenant:       req.GetIdentity().GetTenant(),
		InputType:    req.GetInputType(),
		EnableSparse: enableSparse,
	})
	if err != nil {
		return nil, classifyProviderErr(err)
	}

	// 写回缓存。
	//
	// ★ 键里的维度必须与读取时完全一致，否则缓存永远命中不了。
	//
	//	读取用的是 s.defaultDim（调用前无法得知真实维度），
	//	若这里写 pResp.Dimension，一旦 Provider 实际返回的维度与配置不符，
	//	写与读落在不同键上 —— 缓存静默 100% miss，只表现为成本上升，
	//	不会有任何报错，极难发现。因此统一用维度作为键的"预估值"，
	//	真实维度由 buildEmbedResp 按向量长度计算并返回给调用方。
	if !enableSparse {
		dim := s.defaultDim
		for i, vec := range pResp.Embeddings {
			if i < len(missTexts) {
				s.embedCache.Set(model, dim, req.GetInputType(), missTexts[i], vec)
			}
		}
	}

	s.usage.Record(&UsageRecord{
		RequestID:    "embed",
		UID:          req.GetIdentity().GetUid(),
		Model:        pResp.Model,
		PromptTokens: pResp.Usage.PromptTokens,
	})

	// 稀疏向量与 texts 对齐组装（enable_sparse 时 provider 已返回）
	var sparseFill func() [][]*llmv1.SparseToken
	if enableSparse {
		sparseAll := buildSparseByIndex(texts, pending, pResp.SparseEmbeddings)
		sparseFill = func() [][]*llmv1.SparseToken { return sparseAll }
	}

	return s.buildEmbedResp(model, req, cached, pResp.Usage, func() [][]float32 {
		return mergeEmbeddings(hitVecs, pending, pResp.Embeddings)
	}, sparseFill), nil
}

// buildSparseByIndex 把 provider 返回的稀疏向量（对应 missTexts）展开为
// 与 req.Texts 完整对齐的列表（缓存命中项为 nil，表示无稀疏向量）。
func buildSparseByIndex(texts []string, pending []int, fresh [][]provider.SparseToken) [][]*llmv1.SparseToken {
	full := make([][]*llmv1.SparseToken, len(texts))
	for i, idx := range pending {
		if i >= len(fresh) {
			continue
		}
		src := fresh[i]
		toks := make([]*llmv1.SparseToken, 0, len(src))
		for _, t := range src {
			toks = append(toks, &llmv1.SparseToken{Index: t.Index, Value: t.Value})
		}
		full[idx] = toks
	}
	return full
}

// mergeEmbeddings 把「缓存命中」与「本次向量化」两部分拼成完整有序结果。
//
// full[i] 必须对应 req.Texts[i]，
// 否则 RAG 侧会把 A 文本的向量写进 B 切片 —— 这种错位不会报错，只会静默降低召回。
func mergeEmbeddings(hitVecs [][]float32, pending []int, fresh [][]float32) [][]float32 {
	full := make([][]float32, len(hitVecs))
	copy(full, hitVecs)
	for i, idx := range pending {
		if i < len(fresh) {
			full[idx] = fresh[i]
		}
	}
	return full
}

// buildEmbedResp 构造 Embed 响应
func (s *LLMService) buildEmbedResp(model string, req *llmv1.EmbedReq, cached []bool, usage provider.Usage, fill func() [][]float32, sparseFill func() [][]*llmv1.SparseToken) *llmv1.EmbedResp {
	var vectors [][]float32
	if fill != nil {
		vectors = fill()
	}
	dim := s.defaultDim
	if len(vectors) > 0 && len(vectors[0]) > 0 {
		dim = len(vectors[0])
	}

	out := &llmv1.EmbedResp{
		Model:     model,
		Dimension: int32(dim),
		Usage:     toPBUsage(usage),
		Cached:    cached,
	}
	for i, vec := range vectors {
		if vec == nil {
			continue
		}
		out.Embeddings = append(out.Embeddings, &llmv1.Embedding{
			Vector: vec,
			Index:  int32(i),
		})
	}
	// 稀疏向量按 index 对齐输出
	if sparseFill != nil {
		for i, toks := range sparseFill() {
			if len(toks) == 0 {
				continue
			}
			out.SparseEmbeddings = append(out.SparseEmbeddings, &llmv1.SparseEmbedding{
				Tokens: toks,
				Index:  int32(i),
			})
		}
	}
	return out
}

// ---------- Rerank ----------

// Rerank 候选重排。
//
// 供 rag_service 的检索链路调用：向量召回 topK → Rerank → 取 topN → 拼 Prompt。
//
// ★ 本handler 刻意只做三件事：参数校验、故障注入、错误码映射。
//
//	阈值过滤、topN 截断、上下文外扩都在 rag_service 侧做 ——
//	那些是检索策略而非模型调用语义，放这里会让本服务背上业务决策。
func (s *LLMService) Rerank(ctx context.Context, req *llmv1.RerankReq) (*llmv1.RerankResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	if req.GetQuery() == "" || len(req.GetDocuments()) == 0 {
		return nil, errno.New(errno.ErrMissingParam)
	}

	model := req.GetModel()
	if model == "" {
		model = s.defaultRerankModel
	}
	if s.mock.ShouldFail("rerank") {
		return nil, errno.New(errno.ErrModelUnavailable)
	}

	start := time.Now()
	pResp, err := s.prov.Rerank(ctx, &provider.RerankRequest{
		Model:           model,
		Query:           req.GetQuery(),
		Documents:       req.GetDocuments(),
		TopN:            int(req.GetTopN()),
		UID:             req.GetIdentity().GetUid(),
		Tenant:          req.GetIdentity().GetTenant(),
		ReturnDocuments: req.GetReturnDocuments(),
	})
	if err != nil {
		// ★ 复用同一套映射：IsUnsupported → 501（能力缺失，调用方据此跳过重排），
		//   超时 → 504，其余 → 503（真故障，调用方应重试/降级）。
		//   两者语义必须分开，否则「未配置重排模型」会被当故障反复重试。
		return nil, classifyProviderErr(err)
	}

	s.usage.Record(&UsageRecord{
		RequestID:    "rerank",
		UID:          req.GetIdentity().GetUid(),
		Model:        pResp.Model,
		PromptTokens: pResp.Usage.PromptTokens,
		LatencyMS:    time.Since(start).Milliseconds(),
	})

	out := &llmv1.RerankResp{
		Model: pResp.Model,
		Usage: toPBUsage(pResp.Usage),
	}
	for _, r := range pResp.Results {
		out.Results = append(out.Results, &llmv1.RerankResult{
			Index:          int32(r.Index),
			RelevanceScore: r.RelevanceScore,
			Document:       r.Document,
		})
	}
	return out, nil
}

// ---------- 元信息 ----------

func (s *LLMService) ListModels(ctx context.Context, req *llmv1.ListModelsReq) (*llmv1.ListModelsResp, error) {
	models, err := s.prov.ListModels(ctx)
	if err != nil {
		return nil, classifyProviderErr(err)
	}
	out := &llmv1.ListModelsResp{DefaultModel: s.defaultModel}
	for _, m := range models {
		if req.GetOnlyEnabled() && m.Status != "enabled" && m.Status != "" {
			continue
		}
		out.Models = append(out.Models, &llmv1.ModelInfo{
			Name:              m.Name,
			Provider:          m.Provider,
			BaseUrl:           m.BaseURL,
			MaxTokens:         int32(m.MaxTokens),
			SupportsToolCall:  m.SupportsToolCall,
			SupportsVision:    m.SupportsVision,
			SupportsReasoning: m.SupportsReasoning,
			Status:            m.Status,
			Priority:          int32(m.Priority),
			InputPrice:        m.InputPrice,
			OutputPrice:       m.OutputPrice,
		})
	}
	return out, nil
}

func (s *LLMService) GetCapabilities(_ context.Context, _ *llmv1.GetCapabilitiesReq) (*llmv1.GetCapabilitiesResp, error) {
	caps := s.prov.Capabilities()
	return &llmv1.GetCapabilitiesResp{
		ToolCall:   caps.ToolCall,
		Reasoning:  caps.Reasoning,
		Multimodal: caps.Multimodal,
		Embeddings: caps.Embeddings,
		Provider:   s.prov.Name(),
		IsMock:     s.prov.Name() == "mock",
	}, nil
}

// ---------- 计量 ----------

func (s *LLMService) GetUsage(ctx context.Context, req *llmv1.GetUsageReq) (*llmv1.GetUsageResp, error) {
	records, totalTokens, totalCost := s.usage.Query(req.GetUid(), req.GetStartTs(), req.GetEndTs())
	page, total := paginate(records, req.GetPage())

	out := &llmv1.GetUsageResp{
		Meta: &commonv1.PageMeta{
			Total:    int32(total),
			Page:     int32(page),
			PageSize: int32(pageSize(req.GetPage())),
			HasMore:  page*pageSize(req.GetPage()) < total,
		},
		TotalTokens: totalTokens,
		TotalCost:   totalCost,
	}
	for _, r := range records {
		out.Records = append(out.Records, &llmv1.UsageRecord{
			RequestId:        r.RequestID,
			Uid:              r.UID,
			Model:            r.Model,
			PromptTokens:     r.PromptTokens,
			CompletionTokens: r.CompletionTokens,
			Cost:             r.Cost,
			CreatedAt:        r.CreatedAt,
			Degraded:         r.Degraded,
		})
	}
	return out, nil
}

// ---------- 转换工具 ----------

func (s *LLMService) toProviderRequest(req *llmv1.ChatReq) *provider.ChatRequest {
	msgs := make([]provider.Message, 0, len(req.GetMessages()))
	for _, m := range req.GetMessages() {
		pMsg := provider.Message{
			Role:    provider.Role(m.GetRole()),
			Content: m.GetContent(),
			Name:    m.GetName(),
		}
		// tool 消息需要 tool_call_id（关联到 assistant 消息里的 tool_calls[].id）
		if m.GetRole() == "tool" {
			// 从 Message 里如果有 ToolCalls（assistant 可能带）提取 ID
			for _, tc := range m.GetToolCalls() {
				pMsg.ToolCallID = tc.GetId()
				break
			}
		} else {
			for _, tc := range m.GetToolCalls() {
				pMsg.ToolCallID = tc.GetId()
			}
		}
		msgs = append(msgs, pMsg)
	}

	maxTokens := int(req.GetMaxTokens())
	if maxTokens <= 0 {
		maxTokens = s.maxTokens
	}

	out := &provider.ChatRequest{
		Model:           req.GetModel(),
		Messages:        msgs,
		Temperature:     float64(req.GetTemperature()),
		MaxTokens:       maxTokens,
		EnableReasoning: req.GetEnableReasoning(),
		RequestID:       req.GetRequestId(),
		UID:             req.GetIdentity().GetUid(),
		Tenant:          req.GetIdentity().GetTenant(),
	}

	// 工具名列表 → 完整 Tool 定义
	if names := req.GetTools(); len(names) > 0 {
		if s.toolRes != nil {
			if tools, err := s.toolRes.ResolveTools(context.Background(), names); err == nil {
				out.Tools = tools
			} else {
				logger.Warn(context.Background(), "toolRes 解析工具失败（本轮跳过）", "err", err, "names", names)
			}
		}
	}

	return out
}

func toPBUsage(u provider.Usage) *llmv1.Usage {
	return &llmv1.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CachedTokens:     u.CachedTokens,
	}
}

func degradationOf(degraded bool, reason string) *commonv1.Degradation {
	if !degraded {
		return nil
	}
	return &commonv1.Degradation{
		Degraded: true,
		Reason:   "provider_degraded",
		Detail:   reason,
	}
}

// classifyProviderErr 把 Provider 错误映射为业务错误码
func classifyProviderErr(err error) error {
	if err == nil {
		return nil
	}
	if provider.IsUnsupported(err) {
		return errno.Wrap(errno.ErrNotImplemented, err)
	}
	// ctx 超时优先归类为 timeout，便于上层决定是否重试
	if err == context.DeadlineExceeded {
		return errno.Wrap(errno.ErrTimeout, err)
	}
	return errno.Wrap(errno.ErrModelUnavailable, err)
}

// UsageRecorder 计量记录器
type UsageRecorder struct {
	mu      sync.RWMutex
	records []*UsageRecord
	max     int
}

// UsageRecord 一条计量记录
type UsageRecord struct {
	RequestID        string
	UID              int64
	Model            string
	PromptTokens     int32
	CompletionTokens int32
	Cost             float64
	CreatedAt        int64
	Degraded         bool
	LatencyMS        int64
}

// NewUsageRecorder 构造计量记录器
func NewUsageRecorder(max int) *UsageRecorder {
	if max <= 0 {
		max = 1000
	}
	return &UsageRecorder{max: max}
}

// Record 记录一次调用
func (r *UsageRecorder) Record(rec *UsageRecord) {
	if rec == nil {
		return
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = time.Now().Unix()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
	// 环形淘汰：只保留最近 N 条
	if len(r.records) > r.max {
		r.records = r.records[len(r.records)-r.max:]
	}
}

// Query 查询计量记录
func (r *UsageRecorder) Query(uid int64, startTS, endTS int64) ([]*UsageRecord, int64, float64) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []*UsageRecord
	var totalTokens int64
	var totalCost float64

	for _, rec := range r.records {
		if uid != 0 && rec.UID != uid {
			continue
		}
		if startTS > 0 && rec.CreatedAt < startTS {
			continue
		}
		if endTS > 0 && rec.CreatedAt > endTS {
			continue
		}
		out = append(out, rec)
		totalTokens += int64(rec.PromptTokens + rec.CompletionTokens)
		totalCost += rec.Cost
	}
	// 倒序：最近的调用在前
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, totalTokens, totalCost
}
