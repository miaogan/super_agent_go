package rawhttp

import (
	"strings"
	"testing"
)

// TestRerankEndpoint 锁住 rerank 的路径推导。
//
// ★ 这组用例全部来自实测踩坑：
//
// 该平台（阿里云百炼 compatible-mode）的 rerank **不**在 BaseURL 语义下——
//
//	chat / embeddings → https://host/compatible-mode/v1/v1/chat/completions
//	rerank           → https://host/api/v1/services/rerank/{服务名}/{服务名}
//
// compatible-mode 下试过的 8 种 OpenAI 风格路径（/rerank、/reranks、
// /v1/rerank、/v1/reranks、/rerank/v1、/text-rerank、/v1/text-rerank、
// /api/v1/rerank）全部 404，只有 DashScope 原生路径可用。
func TestRerankEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		service string
		want    string
	}{
		{
			name:    "百炼 compatible-mode（实际使用场景）",
			baseURL: "https://maas.qianwenaiapi.com/compatible-mode/v1",
			service: "text-rerank/text-rerank",
			want:    "https://maas.qianwenaiapi.com/api/v1/services/rerank/text-rerank/text-rerank",
		},
		{
			name:    "BaseURL 末尾多斜杠",
			baseURL: "https://maas.qianwenaiapi.com/compatible-mode/v1/",
			service: "text-rerank/text-rerank",
			want:    "https://maas.qianwenaiapi.com/api/v1/services/rerank/text-rerank/text-rerank",
		},
		{
			name:    "裸域名 BaseURL",
			baseURL: "https://host",
			service: "m/m",
			want:    "https://host/api/v1/services/rerank/m/m",
		},
		{
			name:    "非compatible 前缀也能正确反推",
			baseURL: "https://host/custom/prefix/v1",
			service: "m/m",
			want:    "https://host/api/v1/services/rerank/m/m",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rerankEndpoint(c.baseURL, c.service)
			if got != c.want {
				t.Errorf("rerankEndpoint(%q, %q)\n  got  %q\n  want %q",
					c.baseURL, c.service, got, c.want)
			}
		})
	}
}

// TestRerankEndpointNoCompatibleMode 明确「rerank URL 里不能出现 compatible-mode」——
// 实测带了这个前缀就是 404，这条断言防的是「有人顺手复用endpoint()」的回归。
func TestRerankEndpointNoCompatibleMode(t *testing.T) {
	got := rerankEndpoint("https://maas.qianwenaiapi.com/compatible-mode/v1", "text-rerank/text-rerank")
	if strings.Contains(got, "compatible-mode") {
		t.Fatalf("rerank URL 不应包含 compatible-mode（实测会 404）: %s", got)
	}
	if strings.Contains(got, "/v1/v1/") {
		t.Fatalf("rerank URL 出现重复 /v1: %s", got)
	}
}

// TestRerankEndpointServiceNotModelName 锁死本轮最贵的那个教训：
// **路径段是服务名，不是模型名**，而请求体的 model 字段才是模型名。
//
// 两者字面只差一个 "qwen3.7-" 前缀。实测服务端对前者返回 200、对后者返回
// 400 "url error, please check url" —— 报错完全不提「路径段用错」，
// 只能靠穷举定位，前后花掉十几轮排除。
//
// 这条用例的作用是把「服务名 ≠ 模型名」这一事实固化成可断言的差异：
// 用服务名和用模型名拼出的URL 必须是两个不同的东西，
// 所以必须单独提供配置项，不能指望「同一个字段两用」。
//
// 真正防回归的是 TestRerankUnsupportedWhenNoServicePath ——
// 没配服务名时必须压根不发请求。
func TestRerankEndpointServiceNotModelName(t *testing.T) {
	const base = "https://maas.qianwenaiapi.com/compatible-mode/v1"
	const model = "qwen3.7-text-rerank"
	const service = "text-rerank/text-rerank"

	good := rerankEndpoint(base, service)
	if !strings.HasSuffix(good, "/api/v1/services/rerank/"+service) {
		t.Fatalf("路径段应为服务名 %q，实际 %q", service, good)
	}
	// 用模型名拼出的路径与可用路径不同 —— 实测这些组合全部 400
	for _, wrong := range []string{
		model + "/" + model,
		model + "/" + service,
		service + "/" + model,
	} {
		if bad := rerankEndpoint(base, wrong); bad == good {
			t.Errorf("服务名 %q 与模型名 %q 拼出了相同 URL，配置项失去意义", service, model)
		}
	}
}
