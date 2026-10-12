package rawhttp

import (
	"strings"
	"testing"
)

// TestEndpointBaseURL 锁住 BaseURL 归一化行为。
//
// ★ 这组用例来自实测踩坑：
//
//	百炼 compatible-mode 的文档给的是
//	https://maas.qianwenaiapi.com/compatible-mode/v1
//	照抄进配置后，直连拼出的
//	https://…/compatible-mode/v1/v1/chat/completions
//	返回 404，且本地日志看不出「BaseURL 多写了一段 /v1」。
//
// 因此 endpoint() 必须容忍 BaseURL 末尾已带 /v1 的情况。
func TestEndpointBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		path    string
		want    string
	}{
		{
			name:    "BaseURL 带 /v1（百炼/ OpenAI 常见写法）",
			baseURL: "https://maas.qianwenaiapi.com/compatible-mode/v1",
			path:    "/v1/chat/completions",
			want:    "https://maas.qianwenaiapi.com/compatible-mode/v1/chat/completions",
		},
		{
			name:    "BaseURL 不带 /v1（裸端点）",
			baseURL: "https://maas.qianwenaiapi.com/compatible-mode",
			path:    "/v1/chat/completions",
			want:    "https://maas.qianwenaiapi.com/compatible-mode/v1/chat/completions",
		},
		{
			name:    "BaseURL 末尾多斜杠",
			baseURL: "https://host/v1/",
			path:    "/v1/chat/completions",
			want:    "https://host/v1/chat/completions",
		},
		{
			name:    "embeddings 路径同样适用",
			baseURL: "https://host/v1",
			path:    "/v1/embeddings",
			want:    "https://host/v1/embeddings",
		},
		{
			name:    "非 /v1 前缀的路径不被误改",
			baseURL: "https://host/v1",
			path:    "/v1beta/models/x",
			want:    "https://host/v1/v1beta/models/x",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := endpoint(c.baseURL, c.path); got != c.want {
				t.Errorf("endpoint(%q, %q)\n  got  %q\n  want %q",
					c.baseURL, c.path, got, c.want)
			}
		})
	}
}

// TestEndpointNoDuplicateV1 明确「不能出现 /v1/v1」——
// 这正是实测遇到的 404 根因，单独断言以防回归时被"优化"掉。
func TestEndpointNoDuplicateV1(t *testing.T) {
	got := endpoint("https://maas.qianwenaiapi.com/compatible-mode/v1", "/v1/chat/completions")
	if strings.Contains(got, "/v1/v1") {
		t.Fatalf("URL 出现重复 /v1 段: %s", got)
	}
}
