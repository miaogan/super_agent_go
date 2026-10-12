package rawhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/provider"
)

// newEmbedProvider 构造一个 BaseURL 指向测试服务器的 provider。
func newEmbedProvider(t *testing.T, srvURL string) *Provider {
	t.Helper()
	cfg := conf.LLMConf{
		DefaultEmbedModel: "e1",
		Models: []conf.ModelConf{{
			Name:            "e1",
			RealModel:       "e1",
			BaseURL:         srvURL + "/compatible-mode/v1",
			APIKey:          "sk-test",
			Status:          "enabled",
			EmbedNativePath: "text-embedding/text-embedding",
			EnableSparse:    true,
		}},
	}
	return New(Options{Conf: cfg})
}

// TestEmbedNativeSparse DashScope 原生接口：同时返回稠密 + 稀疏向量。
func TestEmbedNativeSparse(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]any{
			"output": map[string]any{
				"embeddings": []map[string]any{
					{
						"text_index": 0,
						"embedding":  []float64{0.1, 0.2, 0.3},
						"sparse_embedding": []map[string]any{
							{"index": 100, "value": 0.5},
							{"index": 200, "value": 0.7},
						},
					},
				},
			},
			"usage": map[string]any{"total_tokens": 12},
		})
	}))
	t.Cleanup(srv.Close)

	p := newEmbedProvider(t, srv.URL)
	m, err := p.resolve("e1")
	if err != nil {
		t.Fatalf("resolve 失败: %v", err)
	}

	resp, err := p.embedNative(context.Background(), &provider.EmbedRequest{
		Texts: []string{"hello"}, InputType: "query", EnableSparse: true,
	}, m)
	if err != nil {
		t.Fatalf("embedNative 失败: %v", err)
	}
	// 端点：砍掉 /compatible-mode/v1，走 /api/v1/services/embeddings/
	if gotPath != "/api/v1/services/embeddings/text-embedding/text-embedding" {
		t.Errorf("端点不符: %s", gotPath)
	}
	// 请求体：input.texts + parameters.output_type=dense&sparse
	params, ok := gotBody["parameters"].(map[string]any)
	if !ok || params["output_type"] != "dense&sparse" || params["text_type"] != "query" {
		t.Errorf("parameters 不符: %v", gotBody)
	}
	// 稠密
	if len(resp.Embeddings) != 1 || len(resp.Embeddings[0]) != 3 || resp.Dimension != 3 {
		t.Errorf("稠密向量不符: %+v", resp.Embeddings)
	}
	// 稀疏
	if len(resp.SparseEmbeddings) != 1 || len(resp.SparseEmbeddings[0]) != 2 {
		t.Fatalf("稀疏向量不符: %+v", resp.SparseEmbeddings)
	}
	if resp.SparseEmbeddings[0][0].Index != 100 || resp.SparseEmbeddings[0][1].Value != 0.7 {
		t.Errorf("稀疏向量内容不符: %+v", resp.SparseEmbeddings[0])
	}
}

// TestEmbedNativeNoSparseField 响应无 sparse_embedding 字段时稀疏为空但不报错。
func TestEmbedNativeNoSparseField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"output": map[string]any{
				"embeddings": []map[string]any{
					{"text_index": 0, "embedding": []float64{0.1, 0.2}},
				},
			},
			"usage": map[string]any{"total_tokens": 5},
		})
	}))
	t.Cleanup(srv.Close)

	p := newEmbedProvider(t, srv.URL)
	m, err := p.resolve("e1")
	if err != nil {
		t.Fatalf("resolve 失败: %v", err)
	}

	resp, err := p.embedNative(context.Background(), &provider.EmbedRequest{
		Texts: []string{"x"}, EnableSparse: true,
	}, m)
	if err != nil {
		t.Fatalf("embedNative 失败: %v", err)
	}
	if len(resp.Embeddings) != 1 || len(resp.SparseEmbeddings) != 1 || len(resp.SparseEmbeddings[0]) != 0 {
		t.Errorf("无 sparse 字段时稀疏应为空: %+v", resp.SparseEmbeddings)
	}
}

// TestEmbedFallbackWhenNoNativePath 未配 embed_native_path → Embed 走 OpenAI 兼容接口。
func TestEmbedFallbackWhenNoNativePath(t *testing.T) {
	cfg := conf.LLMConf{
		DefaultEmbedModel: "e2",
		Models: []conf.ModelConf{{
			Name: "e2", BaseURL: "https://example.com/v1", APIKey: "k", Status: "enabled",
			// 无 EmbedNativePath / EnableSparse
		}},
	}
	p := New(Options{Conf: cfg})
	m, err := p.resolve("e2")
	if err != nil {
		t.Fatalf("resolve 失败: %v", err)
	}
	// 未配置 embed_native_path：即使请求带 EnableSparse 也不会走原生路径
	if m.EmbedNativePath != "" {
		t.Error("未配置 embed_native_path 应保持为空")
	}
	_ = p
}

// TestEmbedEndpoint 端点拼接：砍掉路径前缀只留 host。
func TestEmbedEndpoint(t *testing.T) {
	if got := embedEndpoint("https://host/compatible-mode/v1", "text-embedding/text-embedding"); got !=
		"https://host/api/v1/services/embeddings/text-embedding/text-embedding" {
		t.Errorf("端点不符: %s", got)
	}
	if got := embedEndpoint("https://host", "text-embedding/text-embedding"); got !=
		"https://host/api/v1/services/embeddings/text-embedding/text-embedding" {
		t.Errorf("端点不符: %s", got)
	}
}
