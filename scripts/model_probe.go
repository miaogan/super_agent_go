//go:build ignore

// 真实连通性验证脚本（不参与编译，手动执行）。
//
// 用途：配置改完 / 换 Key / 换厂商后，确认
//
//	BaseURL 拼接 → 鉴权 → 模型名可用 → 维度与 Milvus schema 一致
//
//	这四件事同时成立。
//
// 用法：
//
//	source scripts/goenv.sh
//	set -a && source .env && set +a
//	go run scripts/model_probe.go
//
// ★ 为什么不让这个成为常驻测试：
//
//	它依赖真实网络与付费额度，CI 里跑必然不稳定。
//	它的定位是「部署前的健康检查」，由人在环境变更时主动执行。
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/jsonx"
)

// endpoint 与 pkg/provider/rawhttp.endpoint 保持一致。
// 这里独立实现是为了让脚本零依赖（不 import rawhttp，避免整套 provider 链路）。
func endpoint(base, path string) string {
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	if len(base) >= 3 && base[len(base)-3:] == "/v1" &&
		len(path) >= 3 && path[:3] == "/v1" {
		path = path[3:]
	}
	return base + path
}

type embedResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
	// Error 用any 而非 jsonx.RawMessage ——
	// jsonx 只暴露两引擎的交集方法集，RawMessage 不是其中之一（这是刻意的设计，
	// 见 pkg/jsonx 的 Encoder/Decoder 注释）。探测脚本不需要精细解析错误体。
	Error any `json:"error"`
}

func main() {
	cfgPath := os.Getenv("AEGIS_CONF_FILE")
	if cfgPath == "" {
		cfgPath = "configs/config.example.yaml"
	}

	c, err := conf.Load(cfgPath)
	if err != nil {
		fmt.Println("✗ 加载配置失败:", err)
		os.Exit(1)
	}

	// 密钥优先取环境变量（与 applyEnv 的覆盖规则一致）
	applyEnvKeys(c.LLM.Models)

	fmt.Printf("配置文件: %s\n", cfgPath)
	fmt.Printf("provider=%s  向量维度期望=%d  Milvus dim=%d\n\n",
		c.LLM.Provider, c.LLM.DefaultEmbedDim, c.Milvus.Dim)

	client := &http.Client{Timeout: 60 * time.Second}
	fail := 0

	for i := range c.LLM.Models {
		m := c.LLM.Models[i]
		kind := classify(m)
		fmt.Printf("[%d] %-32s (%s)\n", i, m.Name, kind)

		if m.APIKey == "" {
			fmt.Println("    ⊘ 跳过：未配置 API_KEY")
			continue
		}
		if m.Status == "disabled" {
			fmt.Println("    ⊘ 跳过：status=disabled")
			continue
		}

		// ★ 只有向量模型做真实探测 —— chat 模型调一次要花钱，
		//   而 embedding 几乎免费且能同时验证 4 件事，性价比最高。
		if kind != "向量" {
			fmt.Println("    ⊘ 跳过：仅探测向量模型（chat 探测需付费，且要真发请求）")
			continue
		}

		dim, err := probeEmbedding(client, m)
		if err != nil {
			fmt.Printf("    ✗ %v\n", err)
			fail++
			continue
		}
		if dim != c.LLM.DefaultEmbedDim {
			fmt.Printf("    ✗ 维度不符：模型返回 %d，配置期望 %d，Milvus dim=%d\n",
				dim, c.LLM.DefaultEmbedDim, c.Milvus.Dim)
			fail++
			continue
		}
		fmt.Printf("    ✓ 向量维度 %d，与配置及 Milvus schema 一致\n", dim)
	}

	fmt.Println()
	if fail > 0 {
		fmt.Printf("✗ %d 项失败\n", fail)
		os.Exit(1)
	}
	fmt.Println("✓ 全部通过")
}

// classify 按模型名判断类型。
//
// ★ 现状说明（实测于阿里云百炼 compatible-mode）：
//
//	qwen3.7-text-embedding-flash → /v1/embeddings 可用，输出 1024 维
//	qwen3.7-text-rerank→  /reranks 与 /v1/reranks 均 404，
//	                       疑似该平台未开放 OpenAI 兼容的 rerank 端点
//	qwen3.8-max / deepseek-* / qwen3.7-flash 等 → 未开通（AccessDenied.Unpurchased）
func classify(m conf.ModelConf) string {
	n := m.Name
	switch {
	case contains(n, "embedding"):
		return "向量"
	case contains(n, "rerank"):
		return "重排"
	default:
		return "对话"
	}
}

func probeEmbedding(client *http.Client, m conf.ModelConf) (int, error) {
	body := map[string]any{
		"model": m.RealModel,
		"input": []string{"Aegis 平台连通性自检"},
	}
	payload, err := jsonx.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("序列化请求失败: %w", err)
	}

	url := endpoint(m.BaseURL, "/v1/embeddings")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.APIKey)

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("请求 %s 失败: %w", url, err)
	}
	defer resp.Body.Close()

	// ★ 不要用 LimitReader 截断响应体：
	//   1024 维浮点数的 JSON 体积约 20–30KB，截到 8KB 会导致
	//   解析报 "unexpected end of JSON input"，而实际上游完全正常。
	//   这里限制 4MB，足够装下批量 embedding 的响应，又能防住异常大响应。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var out embedResp
	if err := jsonx.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("解析响应失败: %w（body: %s）", err, truncate(string(raw), 200))
	}
	if len(out.Data) == 0 {
		return 0, fmt.Errorf("响应无向量数据: %s", truncate(string(raw), 200))
	}
	return len(out.Data[0].Embedding), nil
}

func applyEnvKeys(models []conf.ModelConf) {
	// 与 pkg/conf 的 applyEnv 保持同一套环境变量命名
	keys := []string{
		"AEGIS_LLM__MODELS__0__API_KEY", "AEGIS_LLM__MODELS__1__API_KEY",
		"AEGIS_LLM__MODELS__2__API_KEY", "AEGIS_LLM__MODELS__3__API_KEY",
		"AEGIS_LLM__MODELS__4__API_KEY", "AEGIS_LLM__MODELS__5__API_KEY",
	}
	baseKeys := []string{
		"AEGIS_LLM__MODELS__0__BASE_URL", "AEGIS_LLM__MODELS__1__BASE_URL",
		"AEGIS_LLM__MODELS__2__BASE_URL", "AEGIS_LLM__MODELS__3__BASE_URL",
		"AEGIS_LLM__MODELS__4__BASE_URL", "AEGIS_LLM__MODELS__5__BASE_URL",
	}
	realKeys := []string{
		"AEGIS_LLM__MODELS__0__REAL_MODEL", "AEGIS_LLM__MODELS__1__REAL_MODEL",
		"AEGIS_LLM__MODELS__2__REAL_MODEL", "AEGIS_LLM__MODELS__3__REAL_MODEL",
		"AEGIS_LLM__MODELS__4__REAL_MODEL", "AEGIS_LLM__MODELS__5__REAL_MODEL",
	}
	for i := range models {
		if i < len(keys) {
			if v := os.Getenv(keys[i]); v != "" {
				models[i].APIKey = v
			}
			if v := os.Getenv(baseKeys[i]); v != "" {
				models[i].BaseURL = v
			}
			if v := os.Getenv(realKeys[i]); v != "" {
				models[i].RealModel = v
			}
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
