//go:build ignore

// rerank 真实连通性验证（不参与编译，手动执行）。
//
// 用途：确认 rerank 的路径推导、请求体、响应解析在真实网关上成立。
//
// 用法：
//
//	source scripts/goenv.sh
//	set -a && source .env && set +a
//	go run scripts/rerank_probe.go
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

// 语料刻意包含「明显相关 / 勉强相关 / 完全无关」三类，
// 便于肉眼验证排序是否合理 —— 只看 HTTP 200 不能说明模型真的在工作。
var corpus = []string{
	"RAG是检索增强生成：先从知识库检索相关内容，再把检索结果作为上下文交给大模型生成回答，从而缓解幻觉并支持知识实时更新。",
	"Kubernetes 是一个容器编排系统，负责调度与管理容器化应用。",
	"今天天气晴朗，气温 25 度，适合外出散步。",
	"向量检索通过计算查询向量与文档向量的相似度来召回候选，常见度量有余弦相似度与欧氏距离。",
	"Rerank（重排）是对召回到的候选文档做更精细的相关性打分与排序，通常由Cross-Encoder 实现，效果优于双塔向量检索。",
}

var query = "什么是 RAG？检索增强生成是怎么工作的？"

func main() {
	c, err := conf.Load("configs/config.example.yaml")
	if err != nil {
		fmt.Println("✗ 配置加载失败:", err)
		os.Exit(1)
	}
	applyKeys(c.LLM.Models)

	if !c.RAG.RerankEnabled {
		fmt.Println("⚠ 配置里 rag.rerank_enabled=false，本次仅做连通性探测，不改配置")
	}
	fmt.Printf("重排阈值(独立于向量阈值) = %.2f  重排候选数 = %d\n\n",
		c.RAG.RerankScoreThreshold, c.RAG.RerankTopK)

	p := rawhttp.New(rawhttp.Options{Conf: c.LLM})
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	fmt.Printf("查询: %s\n候选 %d 条:\n", query, len(corpus))
	for i, d := range corpus {
		fmt.Printf("  [%d] %.40s…\n", i, d)
	}
	fmt.Println()

	start := time.Now()
	resp, err := p.Rerank(ctx, &provider.RerankRequest{
		Query:           query,
		Documents:       corpus,
		TopN:            3,
		ReturnDocuments: true,
	})
	if err != nil {
		fmt.Println("✗ Rerank 调用失败:", err)
		os.Exit(1)
	}
	cost := time.Since(start)

	fmt.Printf("耗时 %s  模型 %s  实际返回 %d 条  usage=%d tokens\n\n",
		cost.Round(time.Millisecond), resp.Model, len(resp.Results), resp.Usage.TotalTokens)

	for rank, r := range resp.Results {
		pass := ""
		if r.RelevanceScore >= c.RAG.RerankScoreThreshold {
			pass = "✓ 保留"
		} else {
			pass = "✗ 低于阈值，将被丢弃"
		}
		fmt.Printf("第%d名  分数=%.4f  %s\n", rank+1, r.RelevanceScore, pass)
		fmt.Printf("       原文[%d]: %.52s…\n", r.Index, r.Document)
	}

	// 一致性检查：最高分必须落在最相关的那条语料上
	if len(resp.Results) > 0 {
		top := resp.Results[0].Index
		if top != 0 && top != 4 {
			fmt.Printf("\n⚠ 最高分落在原文[%d]，与预期（0 或 4 为RAG 相关）不符，请人工确认\n", top)
		} else {
			fmt.Printf("\n✓ 最高分命中 RAG 相关语料（原文[%d]），排序合理\n", top)
		}
	}
}

func applyKeys(models []conf.ModelConf) {
	for i := range models {
		if v := os.Getenv(fmt.Sprintf("AEGIS_LLM__MODELS__%d__API_KEY", i)); v != "" {
			models[i].APIKey = v
		}
		if v := os.Getenv(fmt.Sprintf("AEGIS_LLM__MODELS__%d__BASE_URL", i)); v != "" {
			models[i].BaseURL = v
		}
	}
}
