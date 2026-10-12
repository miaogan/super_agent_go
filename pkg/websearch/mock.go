package websearch

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
)

// urlQueryEscape 转义查询串（net/url 封装，供本包内部使用）。
func urlQueryEscape(s string) string {
	return url.QueryEscape(s)
}

// itemsEnvelope 通用搜索 API 响应外壳。
//
// 主流搜索 API（火山引擎网页搜索 / SerpAPI / Brave Search 等）
// 通常把结果放在 items / results / web / organic 等字段下。
// 这里支持最常见的两种：顶层 items 数组，或 {data:{items:[]}} 包裹。
type itemsEnvelope struct {
	Items []Result `json:"items"`
	Data  *struct {
		Items []Result `json:"items"`
	} `json:"data"`
	Results []Result `json:"results"`
}

// parseItems 解析搜索 API 的 JSON 响应。
func parseItems(raw []byte) ([]Result, error) {
	var env itemsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	switch {
	case len(env.Items) > 0:
		return env.Items, nil
	case env.Data != nil && len(env.Data.Items) > 0:
		return env.Data.Items, nil
	case len(env.Results) > 0:
		return env.Results, nil
	}
	// 响应合法但无结果：返回空而非报错（"没搜到"是正常业务结论）
	return nil, nil
}

// ---------- Mock Provider（无 key 环境 / 冒烟测试）----------

// MockProvider 确定性网络检索 mock。
//
// 用 mockx 的词袋相似度对预置语料打分，保证：
//   - 查询与语料相关时返回可预测的结果（冒烟断言稳定）
//   - 与查询无关的语料不命中（行为真实）
type MockProvider struct {
	// corpus 预置语料库
	corpus []Result
}

// NewMockProvider 构造 mock 来源。
func NewMockProvider() *MockProvider {
	corpus := []Result{
		{
			Title:   "RAG 检索增强生成指南",
			URL:     "https://example.com/rag-guide",
			Snippet: "检索增强生成（RAG）结合知识库检索与生成模型，先从知识库检索相关内容再生成回答。",
		},
		{
			Title:   "Milvus 向量数据库文档",
			URL:     "https://example.com/milvus-docs",
			Snippet: "Milvus 是开源的云原生向量数据库，支持 HNSW 近似最近邻检索。",
		},
		{
			Title:   "Kubernetes 容器编排",
			URL:     "https://example.com/k8s",
			Snippet: "Kubernetes 是容器编排平台，负责应用的部署、扩缩容与自愈。",
		},
		{
			Title:   "大模型幻觉问题与缓解",
			URL:     "https://example.com/hallucination",
			Snippet: "大模型幻觉指生成与事实不符的内容，检索增强是缓解幻觉的常用手段。",
		},
	}
	return &MockProvider{corpus: corpus}
}

// Name 实现 Provider.Name。
func (p *MockProvider) Name() string { return "websearch-mock" }

// Search 实现 Provider.Search：按词袋相似度打分排序。
func (p *MockProvider) Search(ctx context.Context, query string, topK int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	terms := tokenize(query)
	if len(terms) == 0 {
		return nil, nil
	}
	out := make([]Result, 0, len(p.corpus))
	for _, c := range p.corpus {
		text := strings.ToLower(c.Title + " " + c.Snippet)
		hit := 0
		for _, t := range terms {
			if strings.Contains(text, t) {
				hit++
			}
		}
		if hit == 0 {
			continue
		}
		r := c
		r.Score = float64(hit) / float64(len(terms))
		out = append(out, r)
	}
	SortByScore(out)
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

// tokenize 简易分词：ASCII 词 + 中文 bigram（与 mockx 语义一致）。
func tokenize(s string) []string {
	var out []string
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r < 128 {
			if isAlnum(r) {
				start := i
				for i+1 < len(runes) && runes[i+1] < 128 && isAlnum(runes[i+1]) {
					i++
				}
				out = append(out, strings.ToLower(string(runes[start:i+1])))
			}
			continue
		}
		if i+1 < len(runes) && runes[i+1] >= 128 {
			out = append(out, string(runes[i:i+2]))
		}
	}
	return out
}

func isAlnum(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}
