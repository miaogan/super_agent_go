// Package websearch 提供网络检索（Web Search）抽象，作为 RAG 混合检索的第三来源。
//
// 设计目标（与 rag_service 多来源检索对齐）：
//   - 网络检索与向量/关键词检索地位平等：都是"来源之一"，
//     任一来源故障都不该影响其他来源产出结果（防单源）。
//   - 网络检索是外部依赖（搜索 API），必须可替换：
//     mockprovider（无 key 环境 / 冒烟测试）与 httpprovider（真实 API）互换。
//   - 真实 API 的端点拉取复用 pkg/fetch（自带 SSRF 防护 + 大小限制），
//     绝不裸调 http.Get —— 防止把内网地址当搜索端点。
package websearch

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aegis/pkg/fetch"
	"github.com/aegis/pkg/ssrf"
)

// Result 单条搜索结果。
type Result struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score"`
}

// Provider 网络检索来源。
type Provider interface {
	// Name 来源标识（日志/调试用），如 "websearch-mock" / "websearch-http"
	Name() string
	// Search 检索网络，返回按相关度降序的结果。
	Search(ctx context.Context, query string, topK int) ([]Result, error)
}

// Search 便捷调用（nil Provider 视为未启用）。
func Search(ctx context.Context, p Provider, query string, topK int) ([]Result, error) {
	if p == nil {
		return nil, nil
	}
	if topK <= 0 {
		topK = 5
	}
	res, err := p.Search(ctx, query, topK)
	if err != nil {
		return nil, err
	}
	if len(res) > topK {
		res = res[:topK]
	}
	return res, nil
}

// ---------- HTTP Provider（真实搜索 API）----------

// HTTPOptions HTTP 搜索端点配置。
type HTTPOptions struct {
	// Endpoint 搜索 API URL。占位符 {query} 与 {top_k} 会被替换，
	// 如 https://api.example.com/search?q={query}&n={top_k}
	Endpoint string
	// APIKey 可选；非空时以 Authorization: Bearer 下发
	APIKey string
	// Timeout 单次检索超时；<=0 回落 5s
	Timeout time.Duration
	// SSRF SSRF 防护策略；零值时用生产默认（禁止内网）
	SSRF ssrf.Policy
	// TopK 默认返回条数
	TopK int
}

// HTTPProvider 通过 HTTP 调用通用搜索 API。
//
// 端点必须支持查询参数 {query} 与 {top_k}，并返回 JSON：
// {"items": [{"title": "...", "url": "...", "snippet": "..."}]}
type HTTPProvider struct {
	opt HTTPOptions
}

// NewHTTPProvider 构造 HTTP 搜索来源。
func NewHTTPProvider(opt HTTPOptions) (*HTTPProvider, error) {
	if strings.TrimSpace(opt.Endpoint) == "" {
		return nil, fmt.Errorf("websearch: 缺少 Endpoint")
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 5 * time.Second
	}
	if opt.TopK <= 0 {
		opt.TopK = 5
	}
	return &HTTPProvider{opt: opt}, nil
}

func (p *HTTPProvider) Name() string { return "websearch-http" }

// Search 实现 Provider.Search。
func (p *HTTPProvider) Search(ctx context.Context, query string, topK int) ([]Result, error) {
	if topK <= 0 {
		topK = p.opt.TopK
	}
	url := strings.ReplaceAll(p.opt.Endpoint, "{query}", queryEscaped(query))
	url = strings.ReplaceAll(url, "{top_k}", fmt.Sprintf("%d", topK))

	reqCtx, cancel := context.WithTimeout(ctx, p.opt.Timeout)
	defer cancel()

	// ★ 复用 pkg/fetch：SSRF 前置校验 + Dial 二次校验 + 响应体大小上限
	data, err := fetch.Fetch(reqCtx, url, fetch.Options{
		Timeout:      p.opt.Timeout,
		MaxSizeBytes: 2 << 20, // 搜索响应最多 2MB
		SSRF:         p.opt.SSRF,
	})
	if err != nil {
		return nil, fmt.Errorf("websearch: 拉取搜索结果失败: %w", err)
	}

	items, err := parseItems(data)
	if err != nil {
		return nil, fmt.Errorf("websearch: 解析搜索结果失败: %w", err)
	}
	if len(items) > topK {
		items = items[:topK]
	}
	return items, nil
}

func queryEscaped(q string) string {
	// 使用 net/url 编码，避免注入搜索端点参数
	return urlQueryEscape(q)
}

// ---------- 结果排序 ----------

// SortByScore 按分数降序排序（稳定）。
func SortByScore(items []Result) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Score > items[j].Score
	})
}
