// Package rawhttp 实现 OpenAI 兼容接口的薄客户端。
//
// 对应 ADR-003 的决策：手写薄客户端（约 300 行），不做完整复制品。
//
// 为什么不用官方 openai-go SDK：
//  1. 零第三方依赖是最后一道防线 —— 认证是全平台安全底座，
//     依赖树越浅，供应链风险越低
//  2. 我们只需要 Chat / ChatStream(纯文本) / Embed 三个端点，
//     SDK 的其余能力（Assistants / Files / 微调 / 向量库）都用不上
//  3. 升级 SDK 带来的 breaking change 成本高于自己维护这几百行
//
// 代价（明确承认）：
//   - 不支持 ToolCall、Reasoning、多模态 —— 这些由 einoProvider 承担
//   - 不支持 Azure/OpenAI 特有参数
//   - SSE 解析只处理 data: 行，够用但不支持厂商私有事件
//
// 因此配置层提供能力矩阵：rawhttp 生效时，Capabilities() 如实返回
// ToolCall=false / Reasoning=false，由上层决定是否切到 eino。
package rawhttp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/jsonx"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/provider"
)

// Provider OpenAI 兼容薄客户端
type Provider struct {
	cfg  conf.LLMConf
	http *http.Client

	// 请求超时（流式另设，见 streamTimeout）
	timeout time.Duration

	reqCount atomic.Int64
}

// Options 构造参数
type Options struct {
	Conf conf.LLMConf
}

// New 构造 provider
func New(opt Options) *Provider {
	timeout := opt.Conf.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Provider{
		cfg: opt.Conf,
		// 不设全局 Timeout：流式响应可能长时间保持连接，
		// 由 ctx 控制生命周期更准确
		http:    &http.Client{Transport: http.DefaultTransport},
		timeout: timeout,
	}
}

var _ provider.Provider = (*Provider)(nil)

// Name 实现标识
func (p *Provider) Name() string { return "rawhttp" }

// Capabilities ★ 如实声明能力子集。
//
// ★ ToolCall=true 自本次改动起生效：
//
//	qwen3.8-max / deepseek-v4-pro-0813 / qwen3.8-27b 等主流模型均支持，
//	我们补全了 chatResponse / streamFrame 的 tool_calls 解析以及
//	请求体 tools 字段序列化，可以启用。
//	Reasoning 仍保持 false（reasoning_content 解析需要独立工作）。
//
// 声明为false 不代表「模型不支持」，而是「本客户端没解析该字段」。
// 上层据此决定是否切换到 einoProvider 或关闭相关特性。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ToolCall:   true,
		Reasoning:  false,
		Multimodal: false,
		Embeddings: true,
		Streaming:  true,
	}
}

// ListModels 从配置读取模型清单
func (p *Provider) ListModels(_ context.Context) ([]provider.ModelInfo, error) {
	out := make([]provider.ModelInfo, 0, len(p.cfg.Models))
	for _, m := range p.cfg.Models {
		out = append(out, provider.ModelInfo{
			Name:              m.Name,
			Provider:          m.Provider,
			BaseURL:           m.BaseURL,
			MaxTokens:         m.MaxTokens,
			SupportsToolCall:  m.SupportsToolCall,
			SupportsVision:    m.SupportsVision,
			SupportsReasoning: m.SupportsReasoning,
			Status:            m.Status,
			Priority:          m.Priority,
			InputPrice:        m.InputPrice,
			OutputPrice:       m.OutputPrice,
		})
	}
	return out, nil
}

// resolve 按逻辑名找模型配置
func (p *Provider) resolve(name string) (conf.ModelConf, error) {
	if name == "" {
		name = p.cfg.DefaultModel
	}
	for _, m := range p.cfg.Models {
		if m.Name == name {
			return m, nil
		}
	}
	return conf.ModelConf{}, fmt.Errorf("rawhttp: 未找到模型 %q", name)
}

// modelName 逻辑名 → 厂商真实模型名
func modelName(m conf.ModelConf) string {
	if m.RealModel != "" {
		return m.RealModel
	}
	return m.Name
}

// ---------- Chat ----------

// Chat 同步对话
func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	m, err := p.resolve(req.Model)
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"model":    modelName(m),
		"messages": toOpenAIMessages(req.Messages),
		"stream":   false,
	}
	if len(req.Tools) > 0 {
		body["tools"] = toOpenAITools(req.Tools)
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	} else if m.Temperature > 0 {
		body["temperature"] = m.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	} else if m.MaxTokens > 0 {
		body["max_tokens"] = m.MaxTokens
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var resp chatResponse
	if err := p.post(ctx, m, "/v1/chat/completions", body, &resp); err != nil {
		return nil, err
	}
	p.reqCount.Add(1)

	out := &provider.ChatResponse{
		Model:        m.Name,
		FinishReason: resp.Choices[0].FinishReason,
		Usage: provider.Usage{
			PromptTokens:     int32(resp.Usage.PromptTokens),
			CompletionTokens: int32(resp.Usage.CompletionTokens),
			TotalTokens:      int32(resp.Usage.TotalTokens),
		},
		Provider: "rawhttp",
	}
	if len(resp.Choices) > 0 {
		out.Content = resp.Choices[0].Message.Content
		for _, tc := range resp.Choices[0].Message.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, provider.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}
	// 总 token 数缺失时兜底计算，避免计量为 0
	if out.Usage.TotalTokens == 0 {
		out.Usage.TotalTokens = out.Usage.PromptTokens + out.Usage.CompletionTokens
	}
	return out, nil
}

// ChatStream 流式对话（SSE）
func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest) (<-chan *provider.Chunk, error) {
	m, err := p.resolve(req.Model)
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"model":    modelName(m),
		"messages": toOpenAIMessages(req.Messages),
		"stream":   true,
	}
	if len(req.Tools) > 0 {
		body["tools"] = toOpenAITools(req.Tools)
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}

	payload, err := jsonx.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint(m.BaseURL, "/v1/chat/completions"), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	p.setHeaders(httpReq, m)

	resp, err := p.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("rawhttp: chat stream 状态 %d: %s", resp.StatusCode, truncate(string(b), 300))
	}

	ch := make(chan *provider.Chunk, 32)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		p.reqCount.Add(1)
		p.consumeSSE(ctx, resp.Body, ch)
	}()
	return ch, nil
}

// consumeSSE 解析 SSE 流。
//
// 只识别 data: 行，[DONE] 结束。够用且不引入解析库。
// tool_calls 增量按 index 累积：SSE 会分多次传输同一个 call 的 id/name/arguments，
// 我们按 index 做 map 累加 arguments 字符串，最后整包送给上层。
func (p *Provider) consumeSSE(ctx context.Context, r io.Reader, ch chan<- *provider.Chunk) {
	scanner := bufio.NewScanner(r)
	// SSE 行可能较长（整段 JSON），放大缓冲区
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var totalUsage provider.Usage
	// toolCalls 按 index 累积的 map，用于跨帧拼接增量
	// （SSE tool_calls 是 split 多次传输的：第一次带 id/name/部分 arguments，
	//   后续帧带继续的 arguments 字符串）
	type toolCallAcc struct {
		id        string
		name      string
		arguments strings.Builder
	}
	toolCalls := map[int]*toolCallAcc{}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var frame streamFrame
		if err := jsonx.Unmarshal([]byte(data), &frame); err != nil {
			// 单帧解析失败不应中断整条流
			logger.Debug(ctx, "rawhttp: 跳过无法解析的 SSE 帧", "err", err)
			continue
		}
		if frame.Usage.TotalTokens > 0 {
			totalUsage = provider.Usage{
				PromptTokens:     int32(frame.Usage.PromptTokens),
				CompletionTokens: int32(frame.Usage.CompletionTokens),
				TotalTokens:      int32(frame.Usage.TotalTokens),
			}
		}
		if len(frame.Choices) == 0 {
			continue
		}

		choice := frame.Choices[0]

		// content delta 照常发送
		if delta := choice.Delta.Content; delta != "" {
			select {
			case ch <- &provider.Chunk{Delta: delta}:
			case <-ctx.Done():
				return
			}
		}

		// tool_calls 增量 —— 累积后发送完整 call
		for _, tc := range choice.Delta.ToolCalls {
			acc, ok := toolCalls[tc.Index]
			if !ok {
				acc = &toolCallAcc{}
				toolCalls[tc.Index] = acc
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				acc.arguments.WriteString(tc.Function.Arguments)
			}
		}
	}

	// 发送累积完成的 tool_calls（一次性给上层）
	if len(toolCalls) > 0 {
		for i := 0; i < len(toolCalls); i++ {
			acc, ok := toolCalls[i]
			if !ok {
				continue
			}
			select {
			case ch <- &provider.Chunk{ToolCallDelta: &provider.ToolCall{
				ID:        acc.id,
				Name:      acc.name,
				Arguments: acc.arguments.String(),
			}}:
			case <-ctx.Done():
				return
			}
		}
	}

	// 末帧带 usage
	select {
	case ch <- &provider.Chunk{Done: true, Usage: totalUsage}:
	case <-ctx.Done():
	}
}

// ---------- Embed ----------

// Embed 向量化
func (p *Provider) Embed(ctx context.Context, req *provider.EmbedRequest) (*provider.EmbedResponse, error) {
	modelName := req.Model
	if modelName == "" {
		modelName = p.cfg.DefaultEmbedModel
	}
	m, err := p.resolve(modelName)
	if err != nil {
		return nil, err
	}

	// ★ 稀疏向量只能走 DashScope 原生接口：OpenAI 兼容 /v1/embeddings
	//   不返回 sparse_embedding（百炼文档明确 output_type=sparse 会被忽略）。
	//   模型声明支持稀疏且配了 embed_native_path 时才走原生路径。
	if req.EnableSparse && m.EnableSparse && m.EmbedNativePath != "" {
		return p.embedNative(ctx, req, m)
	}

	body := map[string]any{
		"model": modelNameOf(m),
		"input": req.Texts,
	}
	// 部分厂商（如 Cohere）用 input_type 区分 query/document
	if req.InputType != "" {
		body["input_type"] = req.InputType
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var resp embedResponse
	if err := p.post(ctx, m, "/v1/embeddings", body, &resp); err != nil {
		return nil, err
	}
	p.reqCount.Add(1)

	embeds := make([][]float32, 0, len(resp.Data))
	dim := 0
	for _, d := range resp.Data {
		vec := make([]float32, len(d.Embedding))
		for i, v := range d.Embedding {
			vec[i] = float32(v)
		}
		if dim == 0 {
			dim = len(vec)
		}
		embeds = append(embeds, vec)
	}

	return &provider.EmbedResponse{
		Embeddings: embeds,
		Model:      m.Name,
		Dimension:  dim,
		Usage: provider.Usage{
			PromptTokens: int32(resp.Usage.PromptTokens),
			TotalTokens:  int32(resp.Usage.PromptTokens),
		},
		Cached: make([]bool, len(req.Texts)),
	}, nil
}

// embedNative 走 DashScope 原生 text-embedding 接口（output_type=both），
// 同时拿到稠密 + 稀疏向量。
func (p *Provider) embedNative(ctx context.Context, req *provider.EmbedRequest, m conf.ModelConf) (*provider.EmbedResponse, error) {
	body := map[string]any{
		"model": modelNameOf(m),
		"input": map[string]any{"texts": req.Texts},
		"parameters": map[string]any{
			"text_type":   req.InputType,  // query / document
			"output_type": "dense&sparse", // DashScope 原生接口取值: dense / sparse / dense&sparse
		},
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var resp nativeEmbedResponse
	url := embedEndpoint(m.BaseURL, m.EmbedNativePath)
	if err := p.postAbs(ctx, url, m, body, &resp); err != nil {
		return nil, err
	}
	p.reqCount.Add(1)

	embeds := make([][]float32, 0, len(resp.Output.Embeddings))
	sparse := make([][]provider.SparseToken, 0, len(resp.Output.Embeddings))
	dim := 0
	for _, e := range resp.Output.Embeddings {
		vec := make([]float32, len(e.Embedding))
		for i, v := range e.Embedding {
			vec[i] = float32(v)
		}
		if dim == 0 {
			dim = len(vec)
		}
		embeds = append(embeds, vec)

		toks := make([]provider.SparseToken, 0, len(e.SparseEmbedding))
		for _, st := range e.SparseEmbedding {
			toks = append(toks, provider.SparseToken{Index: st.Index, Value: float32(st.Value)})
		}
		sparse = append(sparse, toks)
	}

	return &provider.EmbedResponse{
		Embeddings:       embeds,
		SparseEmbeddings: sparse,
		Model:            m.Name,
		Dimension:        dim,
		Usage: provider.Usage{
			PromptTokens: int32(resp.Usage.TotalTokens),
			TotalTokens:  int32(resp.Usage.TotalTokens),
		},
		Cached: make([]bool, len(req.Texts)),
	}, nil
}

// Rerank 重排候选文档。
//
// ★ 端点与 chat/embed 不在同一路径前缀下，这是实测踩出来的：
//
//	chat / embeddings → {base}/v1/chat/completions、{base}/v1/embeddings
//	                   base = https://host/compatible-mode/v1
//	rerank→ https://host/api/v1/services/rerank/{服务名}/{服务名}
//	                   ★ 不带 /compatible-mode，走厂商原生路径
//
// 该平台 compatible-mode 下**不存在** OpenAI 风格的 /reranks 或 /rerank
// （实测 8 种候选路径全部 404），只有 DashScope 原生的
// /api/v1/services/rerank/{服务名}/{服务名} 可用，返回体也是原生格式：
//
//	{"output":{"results":[{"index":0,"relevance_score":0.967}]}}
//
// 路径中的**服务名**由配置项 rerank_path 提供，与请求体的 model 字段
// （完整模型名）不是同一个值 —— 详见 rerankEndpoint 与 conf.ModelConf.RerankPath。
// 所以这里不复用 endpoint() 拼路径，而是从 BaseURL 反推域名。
func (p *Provider) Rerank(ctx context.Context, req *provider.RerankRequest) (*provider.RerankResponse, error) {
	modelName := req.Model
	if modelName == "" {
		modelName = p.rerankModel()
	}
	if modelName == "" {
		return nil, provider.ErrUnsupported
	}
	m, err := p.resolve(modelName)
	if err != nil {
		return nil, err
	}
	if m.RerankPath == "" {
		// 没有配置服务名就不发请求 —— 拿模型名硬拼路径必然 400。
		// 「猜一个」比「不做」更糟：既拿不到结果，又污染错误日志，
		// 还会让上层误以为重排能力异常。
		return nil, provider.ErrUnsupported
	}
	if len(req.Documents) == 0 {
		// 没有候选文档时不必调远程 —— 直接返回空结果，
		// 避免为了「排序 0 条」浪费一次调用
		return &provider.RerankResponse{Results: []provider.RerankResult{}, Model: m.Name}, nil
	}

	// ★ DashScope 原生 rerank 请求体是 input/parameters 包装，
	//   不是 OpenAI 风格的平铺 {model, query, documents} ——
	//   平铺会返回 400 InvalidParameter: Required body invalid。
	//   实测记录见 conf.ModelConf.RerankPath 注释。
	body := map[string]any{
		"model": modelNameOf(m),
		"input": map[string]any{
			"query":     req.Query,
			"documents": req.Documents,
		},
		"parameters": map[string]any{
			"top_n":            req.TopN,
			"return_documents": req.ReturnDocuments,
		},
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	var resp rerankResponse
	url := rerankEndpoint(m.BaseURL, m.RerankPath)
	if err := p.postAbs(ctx, url, m, body, &resp); err != nil {
		return nil, err
	}
	p.reqCount.Add(1)

	out := make([]provider.RerankResult, 0, len(resp.Output.Results))
	for _, r := range resp.Output.Results {
		if r.Index < 0 || r.Index >= len(req.Documents) {
			// 越界说明返回体异常，跳过而不是 panic ——
			// 上层只按 Index 取原文，越界会导致切片越界崩溃
			continue
		}
		item := provider.RerankResult{
			Index:          r.Index,
			RelevanceScore: r.RelevanceScore,
		}
		if req.ReturnDocuments {
			item.Document = req.Documents[r.Index]
		}
		out = append(out, item)
	}

	// TopN 截断（<=0 表示全部）
	if req.TopN > 0 && len(out) > req.TopN {
		out = out[:req.TopN]
	}

	return &provider.RerankResponse{
		Results: out,
		Model:   m.Name,
		Usage: provider.Usage{
			PromptTokens: int32(resp.Usage.PromptTokens),
			TotalTokens:  int32(resp.Usage.TotalTokens),
		},
	}, nil
}

// rerankModel 找出清单里第一个启用的重排模型。
//
// 约定：模型名含 "rerank" 即视为重排模型 —— 与 model_probe.go 的 classify 一致。
// 配置里没有或全部 disabled 时返回空串，Rerank 据此返回 ErrUnsupported，
// 上层自动跳过重排环节（检索链路退化为「纯向量 + 关键词降级」，仍可工作）。
func (p *Provider) rerankModel() string {
	for _, m := range p.cfg.Models {
		if m.Status == "disabled" {
			continue
		}
		if strings.Contains(strings.ToLower(m.Name), "rerank") {
			return m.Name
		}
	}
	return ""
}

// rerankEndpoint 由 BaseURL + 配置里的服务名推导重排请求的完整 URL。
//
// 输入 BaseURL   https://maas.qianwenaiapi.com/compatible-mode/v1
// 输入 service   text-rerank/text-rerank
// 输出           https://maas.qianwenaiapi.com/api/v1/services/rerank/text-rerank/text-rerank
//
// ★ 关键：service 不是模型名。
//
//	DashScope 原生路径的两段都是**服务名**，而请求体的 model 字段才是完整模型名。
//	拿模型名当路径段会被服务端判为路由不存在，返回
//	400 {"code":"InvalidParameter","message":"url error, please check url"}，
//	且该报错不提示「路径段用错」，排查成本很高。
//	详见 conf.ModelConf.RerankPath 的实测记录。
func rerankEndpoint(baseURL, service string) string {
	base := strings.TrimRight(baseURL, "/")
	// 砍掉 compatible-mode 之类的路径前缀，只留 scheme://host
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		base = u.Scheme + "://" + u.Host
	}
	return base + "/api/v1/services/rerank/" + service
}

// postAbs 对绝对 URL 发起请求。
//
// 与 post() 的区别：post() 用 endpoint(m.BaseURL, path) 拼接，
// 而 rerank 的路径不在 BaseURL 的语义范围内，必须自己给全 URL。
func (p *Provider) postAbs(ctx context.Context, fullURL string, m conf.ModelConf, body any, out any) error {
	payload, err := jsonx.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	p.setHeaders(req, m)

	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("rawhttp: rerank 状态 %d: %s", resp.StatusCode, truncate(string(b), 500))
	}
	return jsonx.NewDecoder(resp.Body).Decode(out)
}

// Close 释放空闲连接
func (p *Provider) Close() error {
	if tr, ok := p.http.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	return nil
}

// Stats 返回调用次数
func (p *Provider) Stats() int64 { return p.reqCount.Load() }

// ---------- HTTP 基础 ----------

// endpoint 拼接完整请求 URL，兼容 BaseURL 带/不带 /v1 后缀。
//
// ★ 为什么必须做这个归一化：
//
//	OpenAI 兼容网关的 BaseURL 写法在业界不统一，两种都常见：
//	  https://host/v1        （OpenAI SDK / Dify / 多数网关文档）
//	  https://host           （裸端点，少量平台）
//
//	若直接 BaseURL + "/v1/chat/completions"，前一种会拼出
//	https://host/v1/v1/chat/completions → 404，且报错发生在远端，
//	本地日志看不出「是BaseURL 写重复了」。
//
//	实测踩过：阿里云百炼 compatible-mode（https://…/compatible-mode/v1）
//	正是前一种，照抄文档配 BaseURL 会直接 404。
func endpoint(baseURL, path string) string {
	base := strings.TrimRight(baseURL, "/")
	// path 形如 "/v1/chat/completions"；若 base 已以 /v1 结尾则去掉重复段
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}
	return base + path
}

func (p *Provider) post(ctx context.Context, m conf.ModelConf, path string, body any, out any) error {
	payload, err := jsonx.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint(m.BaseURL, path), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	p.setHeaders(req, m)

	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("rawhttp: %s 状态 %d: %s", path, resp.StatusCode, truncate(string(b), 500))
	}
	return jsonx.NewDecoder(resp.Body).Decode(out)
}

func (p *Provider) setHeaders(req *http.Request, m conf.ModelConf) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if m.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.APIKey)
	}
}

func modelNameOf(m conf.ModelConf) string {
	if m.RealModel != "" {
		return m.RealModel
	}
	return m.Name
}

// embedEndpoint DashScope 原生 text-embedding 端点。
// 与 rerank 同理：路径前缀必须砍掉 compatible-mode，只留 scheme://host，
// 且路径段是「服务名」而非模型名。
func embedEndpoint(baseURL, service string) string {
	base := strings.TrimRight(baseURL, "/")
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		base = u.Scheme + "://" + u.Host
	}
	return base + "/api/v1/services/embeddings/" + service
}

// ---------- OpenAI 协议结构 ----------

type oaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// 工具消息需要 name
	Name string `json:"name,omitempty"`
	// assistant 消息可能带 tool_calls
	ToolCalls []struct {
		ID       string `json:"id,omitempty"`
		Type     string `json:"type,omitempty"`
		Function struct {
			Name      string `json:"name,omitempty"`
			Arguments string `json:"arguments,omitempty"`
		} `json:"function,omitempty"`
	} `json:"tool_calls,omitempty"`
	// tool 消息需要 tool_call_id
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// rerankResponse DashScope 原生 rerank 响应格式。
//
// ★ 不是 OpenAI 格式：OpenAI 的 /reranks 假设返回 data[]，
//
//	而该平台返回 output.results[] + relevance_score，字段名与量纲都不同。
type rerankResponse struct {
	Output struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	} `json:"output"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
	RequestID string `json:"request_id"`
}

type streamFrame struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// SSE tool_calls 是增量分片的，包含 index 标识哪一个 call
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// nativeEmbedResponse DashScope 原生 text-embedding 响应。
//
//	output.embeddings[].embedding        —— 稠密向量
//	output.embeddings[].sparse_embedding —— 稀疏向量（output_type=both 时返回）
//	sparse_embedding 的元素是 {index, value} 对，index 为词表位置。
type nativeEmbedResponse struct {
	Output struct {
		Embeddings []struct {
			TextIndex       int       `json:"text_index"`
			Embedding       []float64 `json:"embedding"`
			SparseEmbedding []struct {
				Index int64   `json:"index"`
				Value float64 `json:"value"`
			} `json:"sparse_embedding"`
		} `json:"embeddings"`
	} `json:"output"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

func toOpenAIMessages(msgs []provider.Message) []oaMessage {
	out := make([]oaMessage, 0, len(msgs))
	for _, m := range msgs {
		om := oaMessage{Role: string(m.Role), Content: m.Content}
		if m.Name != "" {
			om.Name = m.Name
		}
		if m.ToolCallID != "" {
			om.ToolCallID = m.ToolCallID
		}
		out = append(out, om)
	}
	return out
}

// toOpenAITools 把 provider.Tool 列表转成 OpenAI 兼容格式的 tools 数组。
// 格式：[{"type":"function","function":{"name":...,"description":...,"parameters":{...JSON Schema...}}}]
func toOpenAITools(tools []provider.Tool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fn := map[string]any{
			"name":        t.Name,
			"description": t.Description,
		}
		if t.Parameters != "" {
			var params any
			if err := jsonx.Unmarshal([]byte(t.Parameters), &params); err == nil {
				fn["parameters"] = params
			}
		}
		out = append(out, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
