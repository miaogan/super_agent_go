package rawhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/provider"
)

// newRerankProvider 造一个指向测试服务器的 Provider。
//
// ★ 关键手法：BaseURL 写成 httptest 服务器地址 + 一个假的路径前缀，
//
//	rerankEndpoint 会反推出 origin（scheme://host）再拼 /api/v1/...，
//	于是请求正好落在测试服务器上 —— 这样测的是完整逻辑，
//	包括路径推导、请求体、响应解析，而不只是 URL 拼接。
//
// ★ rerank_path 显式给 "text-rerank/text-rerank"（服务名，不是模型名）：
//
//	见 TestRerankEndpointServiceNotModelName。
func newRerankProvider(t *testing.T, srvURL, modelName string, enabled bool) *Provider {
	t.Helper()
	status := "enabled"
	if !enabled {
		status = "disabled"
	}
	cfg := conf.LLMConf{
		Models: []conf.ModelConf{{
			Name:       modelName,
			RealModel:  modelName,
			BaseURL:    srvURL + "/compatible-mode/v1",
			APIKey:     "sk-test",
			Status:     status,
			RerankPath: "text-rerank/text-rerank",
		}},
	}
	return New(Options{Conf: cfg})
}

func TestRerankRealResponseParsing(t *testing.T) {
	var gotPath, gotAuth, gotModel, gotQuery string
	var gotDocs []string
	var gotTopN float64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		// ★ DashScope 原生 rerank 用 input/parameters 包装（非平铺）
		if input, ok := body["input"].(map[string]any); ok {
			gotQuery, _ = input["query"].(string)
			if d, ok := input["documents"].([]any); ok {
				for _, v := range d {
					gotDocs = append(gotDocs, v.(string))
				}
			}
		}
		if params, ok := body["parameters"].(map[string]any); ok {
			gotTopN, _ = params["top_n"].(float64)
		}
		// 返回实测到的真实响应格式（DashScope 原生）
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"output":{"results":[
				{"index":0,"relevance_score":0.9676095495364669},
				{"index":2,"relevance_score":0.285683092225233},
				{"index":1,"relevance_score":0.0034268009819917733}
			]},
			"usage":{"prompt_tokens":254,"total_tokens":254},
			"request_id":"e1ab0543-eee6-9ff9-8f0e-4dea9cf0d19d"
		}`))
	}))
	defer srv.Close()

	p := newRerankProvider(t, srv.URL, "qwen3.7-text-rerank", true)
	defer p.Close()

	resp, err := p.Rerank(context.Background(), &provider.RerankRequest{
		Query:           "什么是RAG",
		Documents:       []string{"RAG是检索增强生成，先检索再生成", "今天天气晴朗适合出行", "机器学习是人工智能的一个分支"},
		ReturnDocuments: true,
	})
	if err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}

	// --- 请求侧 ---
	if !strings.HasSuffix(gotPath, "/api/v1/services/rerank/text-rerank/text-rerank") {
		t.Errorf("路径推导错误: %s", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("鉴权头错误: %q", gotAuth)
	}
	if gotModel != "qwen3.7-text-rerank" {
		t.Errorf("模型名未透传: %q", gotModel)
	}
	if gotQuery != "什么是RAG" {
		t.Errorf("query 未透传: %q", gotQuery)
	}
	if len(gotDocs) != 3 {
		t.Errorf("documents 未透传: %v", gotDocs)
	}
	if gotTopN != 0 {
		// 请求未指定 TopN 时应传 0（DashScope 用默认）
		t.Errorf("TopN 透传异常: %v", gotTopN)
	}

	// --- 响应侧 ---
	if len(resp.Results) != 3 {
		t.Fatalf("结果数不对: %d", len(resp.Results))
	}
	// 实测返回顺序已按相关性降序（0.968 / 0.286 / 0.003）
	if resp.Results[0].Index != 0 || resp.Results[1].Index != 2 || resp.Results[2].Index != 1 {
		t.Errorf("排序结果与实测不符: %+v", resp.Results)
	}
	if d := resp.Results[0].Document; d != "RAG是检索增强生成，先检索再生成" {
		t.Errorf("ReturnDocuments 未回带原文: %q", d)
	}
	if resp.Usage.PromptTokens != 254 {
		t.Errorf("usage 解析错误: %+v", resp.Usage)
	}
}

func TestRerankTopNTruncate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"output":{"results":[
			{"index":0,"relevance_score":0.9},
			{"index":1,"relevance_score":0.8},
			{"index":2,"relevance_score":0.7}
		]},"usage":{"prompt_tokens":10,"total_tokens":10}}`))
	}))
	defer srv.Close()

	p := newRerankProvider(t, srv.URL, "qwen3.7-text-rerank", true)
	defer p.Close()

	resp, err := p.Rerank(context.Background(), &provider.RerankRequest{
		Query:     "q",
		Documents: []string{"a", "b", "c"},
		TopN:      2,
	})
	if err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("TopN 未生效: %d 条", len(resp.Results))
	}
	if resp.Results[0].RelevanceScore != 0.9 {
		t.Errorf("截断后应保留最高分: %v", resp.Results[0].RelevanceScore)
	}
}

// TestRerankEmptyDocuments 零候选不应发起远程调用。
//
// 这是省钱的必要优化：上层检索 0 命中时若仍发请求，
// 会为「排序空数组」白付一次调用。
func TestRerankEmptyDocuments(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(500)
	}))
	defer srv.Close()

	p := newRerankProvider(t, srv.URL, "qwen3.7-text-rerank", true)
	defer p.Close()

	resp, err := p.Rerank(context.Background(), &provider.RerankRequest{
		Query:     "q",
		Documents: nil,
	})
	if err != nil {
		t.Fatalf("空候选不应报错: %v", err)
	}
	if called {
		t.Error("空候选却发起了远程调用（浪费额度）")
	}
	if len(resp.Results) != 0 {
		t.Errorf("应返回空结果: %+v", resp.Results)
	}
}

// TestRerankUnsupportedWhenNoModel 配置里没有可用重排模型 → ErrUnsupported。
//
// ★ 关键：必须返回 ErrUnsupported 而不是普通错误，
//
//	上层据此跳过重排环节，而不是当故障去重试。
func TestRerankUnsupportedWhenNoModel(t *testing.T) {
	cfg := conf.LLMConf{
		Models: []conf.ModelConf{
			{Name: "qwen3.8-max", BaseURL: "https://host/v1", Status: "enabled"},
			{Name: "qwen3.7-text-rerank", BaseURL: "https://host/v1", Status: "disabled"},
		},
	}
	p := New(Options{Conf: cfg})
	defer p.Close()

	_, err := p.Rerank(context.Background(), &provider.RerankRequest{
		Query:     "q",
		Documents: []string{"a"},
	})
	if err == nil {
		t.Fatal("没有可用重排模型时应返回错误")
	}
	if !provider.IsUnsupported(err) {
		t.Errorf("应返回 ErrUnsupported（上层据此跳过重排），实际: %v", err)
	}
}

// TestRerankOutOfRangeIndexSkipped 响应里越界的 index 必须跳过而不是 panic。
//
// 上层按Index 去查切片表，越界会导致切片越界崩溃 ——
// 宁可少一条结果，也不能让上游崩。
func TestRerankOutOfRangeIndexSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"output":{"results":[
			{"index":0,"relevance_score":0.9},
			{"index":99,"relevance_score":0.5},
			{"index":-1,"relevance_score":0.4}
		]},"usage":{"prompt_tokens":10,"total_tokens":10}}`))
	}))
	defer srv.Close()

	p := newRerankProvider(t, srv.URL, "qwen3.7-text-rerank", true)
	defer p.Close()

	resp, err := p.Rerank(context.Background(), &provider.RerankRequest{
		Query:           "q",
		Documents:       []string{"a"},
		ReturnDocuments: true,
	})
	if err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("越界项应被跳过，只应剩 1 条: %+v", resp.Results)
	}
	if resp.Results[0].Index != 0 {
		t.Errorf("应保留合法项: %+v", resp.Results[0])
	}
}

// TestRerankAutoPicksFirstEnabledModel 不指定 Model 时自动挑启用中的重排模型。
func TestRerankAutoPicksFirstEnabledModel(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		_, _ = w.Write([]byte(`{"output":{"results":[{"index":0,"relevance_score":1}]},"usage":{}}`))
	}))
	defer srv.Close()

	cfg := conf.LLMConf{
		Models: []conf.ModelConf{
			{Name: "qwen3.8-max", RealModel: "qwen3.8-max", BaseURL: srv.URL + "/compatible-mode/v1", Status: "enabled"},
			{Name: "first-rerank", RealModel: "first-rerank", BaseURL: srv.URL + "/compatible-mode/v1", Status: "enabled", RerankPath: "text-rerank/text-rerank"},
			{Name: "second-rerank", RealModel: "second-rerank", BaseURL: srv.URL + "/compatible-mode/v1", Status: "enabled", RerankPath: "text-rerank/text-rerank"},
		},
	}
	p := New(Options{Conf: cfg})
	defer p.Close()

	_, err := p.Rerank(context.Background(), &provider.RerankRequest{
		Query:     "q",
		Documents: []string{"a"},
	})
	if err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}
	if gotModel != "first-rerank" {
		t.Errorf("应自动选第一个启用的重排模型，实际用了 %q", gotModel)
	}
}

// TestRerankUnsupportedWhenNoServicePath 没配 rerank_path 时不发请求。
//
// ★ 这是本轮排查 400 时确立的取舍：宁可不做，也不猜。
//
// 实测把模型名当路径段拼 URL，服务端恒返回
// 400 {"code":"InvalidParameter","message":"url error, please check url"}，
// 且报错完全不提「路径段用错」。所以配置缺 rerank_path 时
// 直接判定为「不具备重排能力」，而不是硬拼一个必然失败的 URL ——
// 后者既拿不到结果，又在错误日志里留下误导性的 400。
func TestRerankUnsupportedWhenNoServicePath(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"code":"InvalidParameter","message":"url error, please check url！"}`))
	}))
	defer srv.Close()

	// 故意不填 RerankPath
	cfg := conf.LLMConf{
		Models: []conf.ModelConf{{
			Name: "qwen3.7-text-rerank", RealModel: "qwen3.7-text-rerank",
			BaseURL: srv.URL + "/compatible-mode/v1", APIKey: "sk-test", Status: "enabled",
		}},
	}
	p := New(Options{Conf: cfg})
	defer p.Close()

	_, err := p.Rerank(context.Background(), &provider.RerankRequest{
		Query:     "q",
		Documents: []string{"a"},
	})
	if !provider.IsUnsupported(err) {
		t.Errorf("未配服务名应返回 ErrUnsupported，实际: %v", err)
	}
	if called {
		t.Error("未配服务名却发起了请求（拼出来的 URL 必然 400）")
	}
}
