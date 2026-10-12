// Package mockprovider 提供进程内 mock 模型实现。
//
// 冒烟测试要求：不连外部 LLM 也能完整验证 agent_service / rag_service 的
// 全链路（检索 → 拼上下文 → 生成 → 流式输出 → 计量）。
//
// 因此 mock 实现必须"像真的"：
//   - Embed 返回维度正确、确定性可复现的向量
//     （rag_service 的余弦相似度检索才有意义）
//   - ChatStream 真的分片推送，而不是一次性返回
//     （agent_service 的流式拼装逻辑才能被验证）
//   - 返回可预测文本，便于冒烟脚本断言
package mockprovider

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aegis/pkg/mockx"
	"github.com/aegis/pkg/provider"
)

// Provider mock 实现
type Provider struct {
	dim        int
	chatModel  string
	embedModel string

	// chunkDelay 控制流式分片间隔，用于验证前端增量渲染
	chunkDelay time.Duration

	// counters 计量计数器
	reqCount   atomic.Int64
	tokenCount atomic.Int64

	// degrade 模拟降级：调用时返回 degraded 标记
	degrade bool
}

// Options 构造参数
type Options struct {
	Dim        int
	ChatModel  string
	EmbedModel string
	ChunkDelay time.Duration
}

// New 构造 mock provider
func New(opt Options) *Provider {
	if opt.Dim <= 0 {
		opt.Dim = 1024 // 与 bge-m3 保持一致
	}
	if opt.ChatModel == "" {
		opt.ChatModel = "mock-chat"
	}
	if opt.EmbedModel == "" {
		opt.EmbedModel = "mock-embed"
	}
	return &Provider{
		dim:        opt.Dim,
		chatModel:  opt.ChatModel,
		embedModel: opt.EmbedModel,
		chunkDelay: opt.ChunkDelay,
	}
}

var _ provider.Provider = (*Provider)(nil)

// Name 实现标识
func (p *Provider) Name() string { return "mock" }

// Capabilities mock 声明最完整的能力。
//
// 目的：让上层在 mock 模式下走与生产一致的代码分支
// （例如 agent_service 仍会执行工具调用循环），
// 而不是走一条"mock 专用简化路径"导致覆盖率不足。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ToolCall:   true,
		Reasoning:  true,
		Multimodal: false,
		Embeddings: true,
		Streaming:  true,
	}
}

// Models 返回 mock 模型清单
func (p *Provider) Models() []provider.ModelInfo {
	return []provider.ModelInfo{
		{
			Name:              p.chatModel,
			Provider:          "mock",
			BaseURL:           "mock://aegis",
			MaxTokens:         8192,
			SupportsToolCall:  true,
			SupportsReasoning: true,
			Status:            "enabled",
			Priority:          1,
			InputPrice:        0,
			OutputPrice:       0,
		},
		{
			Name:              p.embedModel,
			Provider:          "mock",
			BaseURL:           "mock://aegis",
			MaxTokens:         0,
			SupportsToolCall:  false,
			SupportsReasoning: false,
			Status:            "enabled",
			Priority:          1,
		},
	}
}

// ListModels 列出模型
func (p *Provider) ListModels(_ context.Context) ([]provider.ModelInfo, error) {
	return p.Models(), nil
}

// Chat 同步对话
func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	p.reqCount.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	prompt := lastUserContent(req.Messages)
	answer := p.composeAnswer(req, prompt)
	usage := p.mockUsage(req, answer)

	p.tokenCount.Add(int64(usage.TotalTokens))

	return &provider.ChatResponse{
		Content:      answer,
		Model:        firstNonEmpty(req.Model, p.chatModel),
		FinishReason: "stop",
		Usage:        usage,
		Provider:     "mock",
	}, nil
}

// ChatStream 流式对话。
//
// 真分片：按中文标点与空格切分，逐片推送。
// 这样 agent_service 的 delta 拼装与 SSE 转发逻辑被真实覆盖。
func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest) (<-chan *provider.Chunk, error) {
	p.reqCount.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	prompt := lastUserContent(req.Messages)
	answer := p.composeAnswer(req, prompt)
	usage := p.mockUsage(req, answer)
	ch := make(chan *provider.Chunk, 16)

	go func() {
		defer close(ch)
		defer p.tokenCount.Add(int64(usage.TotalTokens))

		// 逐片推送
		for _, piece := range splitForStream(answer) {
			if p.chunkDelay > 0 {
				select {
				case <-time.After(p.chunkDelay):
				case <-ctx.Done():
					return
				}
			}
			select {
			case ch <- &provider.Chunk{Delta: piece}:
			case <-ctx.Done():
				return
			}
		}

		// 末帧：携带 usage
		select {
		case ch <- &provider.Chunk{Done: true, Usage: usage}:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

// Embed 向量化。
//
// ★ 关键设计：向量由文本内容确定性派生。
//
//	相同文本 → 相同向量 → 相似文本的检索排序稳定可复现，
//	冒烟脚本才能断言"某文档应排第一"。
func (p *Provider) Embed(ctx context.Context, req *provider.EmbedRequest) (*provider.EmbedResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Texts) == 0 {
		return nil, fmt.Errorf("embed: 输入为空")
	}
	p.reqCount.Add(1)

	embeds := make([][]float32, 0, len(req.Texts))
	var sparse [][]provider.SparseToken
	if req.EnableSparse {
		sparse = make([][]provider.SparseToken, 0, len(req.Texts))
	}
	var promptTokens int32
	for _, text := range req.Texts {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		embeds = append(embeds, mockx.FakeEmbedding(p.dim, text))
		if req.EnableSparse {
			st := mockx.SparseFromText(text)
			toks := make([]provider.SparseToken, 0, len(st))
			for _, t := range st {
				toks = append(toks, provider.SparseToken{Index: t.Index, Value: t.Value})
			}
			sparse = append(sparse, toks)
		}
		// 粗略按 1 token / 1.5 字符估算，让计量看起来合理
		promptTokens += int32(len([]rune(text))/2 + 1)
	}

	usage := provider.Usage{
		PromptTokens: promptTokens,
		TotalTokens:  promptTokens,
	}
	p.tokenCount.Add(int64(usage.TotalTokens))

	return &provider.EmbedResponse{
		Embeddings:       embeds,
		SparseEmbeddings: sparse,
		Model:            firstNonEmpty(req.Model, p.embedModel),
		Dimension:        p.dim,
		Usage:            usage,
		Cached:           make([]bool, len(req.Texts)),
	}, nil
}

// Rerank 用词袋余弦相似度模拟重排。
//
// ★ 为什么 mock 要实现 Rerank 而不是直接返回 ErrUnsupported：
//
//	返回不支持的话，冒烟测试里 rerank 这一环永远不会被执行，
//	「向量检索 → 重排 → 取topN」这条链路就断了保护 ——
//	等接上真实 rerank 时才发现前面的排序逻辑有问题，为时已晚。
//
//	用与 Embed 相同的 FakeEmbedding（词袋向量）算余弦，
//	保证 mock 下 rerank 的输入输出结构与真实实现一致。
func (p *Provider) Rerank(ctx context.Context, req *provider.RerankRequest) (*provider.RerankResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Documents) == 0 {
		return &provider.RerankResponse{Results: []provider.RerankResult{}, Model: firstNonEmpty(req.Model, "mock-rerank")}, nil
	}
	p.reqCount.Add(1)

	qVec := mockx.FakeEmbedding(p.dim, req.Query)
	out := make([]provider.RerankResult, 0, len(req.Documents))
	var promptTokens int32
	for i, doc := range req.Documents {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		dVec := mockx.FakeEmbedding(p.dim, doc)
		out = append(out, provider.RerankResult{
			Index:          i,
			Document:       doc,
			RelevanceScore: cosine(qVec, dVec),
		})
		promptTokens += int32(len([]rune(req.Query))/2 + len([]rune(doc))/2 + 1)
	}

	// 按相关性降序（稳定排序：同分保持原顺序，便于冒烟断言可复现）
	sort.SliceStable(out, func(a, b int) bool {
		return out[a].RelevanceScore > out[b].RelevanceScore
	})
	if req.TopN > 0 && len(out) > req.TopN {
		out = out[:req.TopN]
	}

	usage := provider.Usage{PromptTokens: promptTokens, TotalTokens: promptTokens}
	p.tokenCount.Add(int64(usage.TotalTokens))

	return &provider.RerankResponse{
		Results: out,
		Model:   firstNonEmpty(req.Model, "mock-rerank"),
		Usage:   usage,
	}, nil
}

// cosine 余弦相似度
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		if i >= len(b) {
			break
		}
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Close 无资源可释放
func (p *Provider) Close() error { return nil }

// Stats 返回调用统计（供健康检查输出）
func (p *Provider) Stats() (requests, tokens int64) {
	return p.reqCount.Load(), p.tokenCount.Load()
}

// ---------- 内部辅助 ----------

// composeAnswer 拼装 mock 回答。
//
// 回答中回显引用数量与问题摘要，是为了让冒烟脚本能验证
// "RAG 上下文确实被拼进去了"这一点 —— 如果拼装逻辑坏了，
// 回答里就不会出现"依据 N 处引用资料"。
func (p *Provider) composeAnswer(req *provider.ChatRequest, prompt string) string {
	var sb strings.Builder

	// 统计引用标记数量：agent_service 拼上下文时会给每段资料加 [n] 编号
	refCount := 0
	for _, m := range req.Messages {
		refCount += countCitations(m.Content)
	}

	sb.WriteString("【mock 回答】")
	if refCount > 0 {
		sb.WriteString(fmt.Sprintf(" 依据 %d 处引用资料", refCount))
	} else {
		sb.WriteString(" 未使用检索资料")
	}
	sb.WriteString(fmt.Sprintf("。问题「%s」", truncateRunes(prompt, 40)))
	sb.WriteString("；已由 mockProvider 生成回复，用于链路验证。")

	if req.EnableReasoning && p.Capabilities().Reasoning {
		// mock 模式下推理链不单独产出（composeAnswer 已含完整语义），
		// 但请求被记录，说明上层确实走了 reasoning 分支
		sb.WriteString("（已启用推理模式）")
	}
	return sb.String()
}

func (p *Provider) mockUsage(req *provider.ChatRequest, answer string) provider.Usage {
	prompt := 0
	for _, m := range req.Messages {
		prompt += len([]rune(m.Content))/2 + 1
	}
	completion := len([]rune(answer))/2 + 1
	return provider.Usage{
		PromptTokens:     int32(prompt),
		CompletionTokens: int32(completion),
		TotalTokens:      int32(prompt + completion),
	}
}

// splitForStream 把文本切成适合流式推送的片段。
//
// 优先在中文标点与空格处断开，避免把词切碎影响前端显示。
func splitForStream(text string) []string {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	pieces := make([]string, 0, len(runes)/8+1)

	// 断点集合：中英文句读 + 空格
	isBreak := func(r rune) bool {
		switch r {
		case '。', '，', '；', '、', '！', '？', '：', '」', '】', '.', ',', ';', ' ', '\n':
			return true
		}
		return false
	}

	start := 0
	for i := 0; i < len(runes); i++ {
		if !isBreak(runes[i]) {
			continue
		}
		pieces = append(pieces, string(runes[start:i+1]))
		start = i + 1
	}
	if start < len(runes) {
		pieces = append(pieces, string(runes[start:]))
	}
	return pieces
}

func lastUserContent(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == provider.RoleUser {
			return msgs[i].Content
		}
	}
	if len(msgs) > 0 {
		return msgs[len(msgs)-1].Content
	}
	return ""
}

func lastSystemContent(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == provider.RoleSystem {
			return msgs[i].Content
		}
	}
	return ""
}

// countCitations 统计形如 "[1]" "[12]" 的引用标记数量。
//
// 用正则而非 strings.Count("[")：后者会把正文里的方括号（如数组下标、
// Markdown 链接语法）误算成引用。
func countCitations(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '[' {
			continue
		}
		j := i + 1
		digits := 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
			digits++
		}
		if digits > 0 && j < len(s) && s[j] == ']' {
			n++
			i = j
		}
	}
	return n
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
