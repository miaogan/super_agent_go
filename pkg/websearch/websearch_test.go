package websearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aegis/pkg/ssrf"
)

// ---------- Mock Provider ----------

// TestMockProviderHits 相关查询命中可预测结果。
func TestMockProviderHits(t *testing.T) {
	p := NewMockProvider()
	res, err := p.Search(context.Background(), "RAG 检索增强", 5)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("相关查询应有命中")
	}
	// 结果按分数降序
	for i := 1; i < len(res); i++ {
		if res[i].Score > res[i-1].Score {
			t.Errorf("应降序: [%d]=%v > [%d]=%v", i, res[i].Score, i-1, res[i-1].Score)
		}
	}
	if res[0].URL == "" {
		t.Error("结果应含 URL")
	}
}

// TestMockProviderNoHit 无关查询返回空（不是错误）。
func TestMockProviderNoHit(t *testing.T) {
	p := NewMockProvider()
	res, err := p.Search(context.Background(), "zzzzz不存在的词", 5)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("无关查询应返回空: %d 条", len(res))
	}
	// 空查询
	if res, _ := p.Search(context.Background(), "", 5); len(res) != 0 {
		t.Error("空查询应返回空")
	}
}

// TestMockProviderTopK 限制返回条数。
func TestMockProviderTopK(t *testing.T) {
	p := NewMockProvider()
	res, _ := p.Search(context.Background(), "检索 向量 数据库 生成 编排", 2)
	if len(res) > 2 {
		t.Errorf("topK=2 应最多返回 2 条: %d", len(res))
	}
}

// TestSearchNilProvider nil Provider 视为未启用。
func TestSearchNilProvider(t *testing.T) {
	res, err := Search(context.Background(), nil, "q", 5)
	if err != nil || res != nil {
		t.Errorf("nil Provider 应返回 nil/nil: %v %v", res, err)
	}
}

// ---------- HTTP Provider ----------

// testSearchServer 造一个返回固定结果的搜索 API。
func testSearchServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestHTTPProviderSuccess 正常拉取并解析 items。
func TestHTTPProviderSuccess(t *testing.T) {
	srv := testSearchServer(t, []byte(`{"items":[
		{"title":"标题A","url":"https://a.example.com","snippet":"片段A"},
		{"title":"标题B","url":"https://b.example.com","snippet":"片段B"}
	]}`))

	p, err := NewHTTPProvider(HTTPOptions{
		Endpoint: srv.URL + "?q={query}&n={top_k}",
		Timeout:  3 * time.Second,
		SSRF:     ssrf.Policy{AllowPrivateIPs: true}, // httptest 在回环上
	})
	if err != nil {
		t.Fatalf("NewHTTPProvider 失败: %v", err)
	}
	res, err := p.Search(context.Background(), "hello", 2)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(res) != 2 || res[0].Title != "标题A" || res[1].URL != "https://b.example.com" {
		t.Errorf("解析不符: %+v", res)
	}
}

// TestHTTPProviderDataWrapped 兼容 {data:{items:[]}} 包裹结构。
func TestHTTPProviderDataWrapped(t *testing.T) {
	srv := testSearchServer(t, []byte(`{"data":{"items":[
		{"title":"T","url":"https://c.example.com","snippet":"S"}
	]}}`))

	p, _ := NewHTTPProvider(HTTPOptions{
		Endpoint: srv.URL,
		Timeout:  3 * time.Second,
		SSRF:     ssrf.Policy{AllowPrivateIPs: true},
	})
	res, err := p.Search(context.Background(), "q", 5)
	if err != nil || len(res) != 1 || res[0].Title != "T" {
		t.Errorf("data 包裹解析不符: %v %v", res, err)
	}
}

// TestHTTPProviderEmpty 合法但空结果不是错误。
func TestHTTPProviderEmpty(t *testing.T) {
	srv := testSearchServer(t, []byte(`{"items":[]}`))
	p, _ := NewHTTPProvider(HTTPOptions{
		Endpoint: srv.URL,
		Timeout:  3 * time.Second,
		SSRF:     ssrf.Policy{AllowPrivateIPs: true},
	})
	res, err := p.Search(context.Background(), "q", 5)
	if err != nil || len(res) != 0 {
		t.Errorf("空结果应返回空且无错误: %v %v", res, err)
	}
}

// TestHTTPProviderBadJSON 非法 JSON → 报错。
func TestHTTPProviderBadJSON(t *testing.T) {
	srv := testSearchServer(t, []byte(`{"items":`))
	p, _ := NewHTTPProvider(HTTPOptions{
		Endpoint: srv.URL,
		Timeout:  3 * time.Second,
		SSRF:     ssrf.Policy{AllowPrivateIPs: true},
	})
	if _, err := p.Search(context.Background(), "q", 5); err == nil {
		t.Error("非法 JSON 应报错")
	}
}

// TestHTTPProviderSSRFBlocked 内网搜索端点应被 SSRF 拦截。
func TestHTTPProviderSSRFBlocked(t *testing.T) {
	p, err := NewHTTPProvider(HTTPOptions{
		Endpoint: "http://127.0.0.1:1/search", // 生产策略：禁止内网
		Timeout:  2 * time.Second,
		SSRF:     ssrf.Default,
	})
	if err != nil {
		t.Fatalf("NewHTTPProvider 失败: %v", err)
	}
	_, err = p.Search(context.Background(), "q", 5)
	if err == nil || !strings.Contains(err.Error(), "被拦截") {
		t.Fatalf("内网端点应被 SSRF 拦截: %v", err)
	}
}

// TestHTTPProviderMissingEndpoint 缺 Endpoint → 构造报错。
func TestHTTPProviderMissingEndpoint(t *testing.T) {
	if _, err := NewHTTPProvider(HTTPOptions{}); err == nil {
		t.Error("缺 Endpoint 应报错")
	}
}

// TestQueryEscaped 占位符替换与查询转义。
func TestQueryEscaped(t *testing.T) {
	if got := queryEscaped("a b&c"); got != "a+b%26c" {
		t.Errorf("转义不符: %q", got)
	}
}

// TestParseItemsResults 兼容 results 字段。
func TestParseItemsResults(t *testing.T) {
	items, err := parseItems([]byte(`{"results":[{"title":"X","url":"u","snippet":"s"}]}`))
	if err != nil || len(items) != 1 || items[0].Title != "X" {
		t.Errorf("results 字段解析不符: %v %v", items, err)
	}
}

// TestSortByScore 分数降序排序稳定。
func TestSortByScore(t *testing.T) {
	items := []Result{{Score: 0.2, Title: "a"}, {Score: 0.9, Title: "b"}, {Score: 0.5, Title: "c"}}
	SortByScore(items)
	want := []string{"b", "c", "a"}
	for i, w := range want {
		if items[i].Title != w {
			t.Errorf("排序不符: [%d]=%q 期望 %q", i, items[i].Title, w)
		}
	}
}
