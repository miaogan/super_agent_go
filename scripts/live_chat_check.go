//go:build ignore

// 真实模型链路验证（不参与编译，手动执行）。
//
// 与 model_probe.go 的区别：
//
//	model_probe.go → 用裸 http 调，验证「账号 + 模型 + 维度」
//	本脚本        → 走项目自己的 rawHTTPProvider，验证「我们的代码链路」
//
// 后者才是真正有价值的：它同时检验
//
//	endpoint()归一化 → jsonx 序列化 → 请求体格式 → 响应解析 → ChatResponse 组装
//
// 任何一环与厂商协议对不上都会在这里暴露，而不是等到生产。
//
// 用法：
//
//	source scripts/goenv.sh
//	set -a && source .env && set +a
//	go run scripts/live_chat_check.go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/provider"
	"github.com/aegis/pkg/provider/rawhttp"
)

func main() {
	c, err := conf.Load("configs/config.example.yaml")
	if err != nil {
		fmt.Println("✗ 配置加载失败:", err)
		os.Exit(1)
	}
	applyKeys(c.LLM.Models)

	p := rawhttp.New(rawhttp.Options{Conf: c.LLM})
	defer p.Close()

	fmt.Printf("provider=%s  capabilities=%+v\n\n", p.Name(), p.Capabilities())

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 只测enabled 且非向量/重排的模型 —— 向量已由 model-probe 覆盖
	var targets []conf.ModelConf
	for _, m := range c.LLM.Models {
		if m.Status != "disabled" && !isSpecial(m.Name) {
			targets = append(targets, m)
		}
	}

	pass, fail := 0, 0
	for _, m := range targets {
		fmt.Printf("[%s]\n", m.Name)
		req := &provider.ChatRequest{
			Messages: []provider.Message{
				{Role: "user", Content: "用一句话回答：1+1等于几？"},
			},
			MaxTokens:   512, // ★ 给足，避免 reasoning 吃满额度导致 content 为空
			Temperature: 0.3,
		}

		start := time.Now()
		resp, err := p.Chat(ctx, req)
		cost := time.Since(start)

		if err != nil {
			fmt.Printf("    ✗ %v（耗时 %s）\n\n", err, cost.Round(time.Millisecond))
			fail++
			continue
		}
		if resp.Content == "" {
			fmt.Printf("    ⚠ 调用成功但内容为空（耗时 %s）\n", cost.Round(time.Millisecond))
			fmt.Printf("      reasoning长度=%d  usage=%+v\n", len(resp.Reasoning), resp.Usage)
			fmt.Printf("      ★ 若 MaxTokens 偏小，reasoning 会吃满额度导致 content 空\n")
			fail++
			continue
		}
		fmt.Printf("    ✓ 耗时 %s  内容: %s\n", cost.Round(time.Millisecond), truncate(resp.Content, 90))
		fmt.Printf("      usage: in=%d out=%d total=%d\n",
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
		if resp.Reasoning != "" {
			fmt.Printf("      reasoning: %s\n", truncate(resp.Reasoning, 60))
		}
		pass++
		fmt.Println()
	}

	fmt.Printf("通过 %d  失败 %d\n", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
}

func isSpecial(name string) bool {
	return contains(name, "embedding") || contains(name, "rerank")
}

func applyKeys(models []conf.ModelConf) {
	for i := range models {
		if v := os.Getenv(envName(i, "API_KEY")); v != "" {
			models[i].APIKey = v
		}
		if v := os.Getenv(envName(i, "BASE_URL")); v != "" {
			models[i].BaseURL = v
		}
	}
}

func envName(idx int, field string) string {
	return fmt.Sprintf("AEGIS_LLM__MODELS__%d__%s", idx, field)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
