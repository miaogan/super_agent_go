// Package providerfactory 集中决定使用哪个 Provider 实现。
//
// 把选择逻辑从 main 中抽出，理由：
//   - 选择规则（mock 优先、能力匹配、是否需要降级链）有实际逻辑，
//     放在 main 里会变成一段无法单测的代码
//   - einoProvider 落地时只需改这一个文件
package providerfactory

import (
	"context"
	"fmt"
	"strings"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/provider"
	"github.com/aegis/pkg/provider/mockprovider"
	"github.com/aegis/pkg/provider/rawhttp"
)

// Build 按配置构建 Provider。
//
// 规则（与代码一一对应，此前写着"eino + 多模型 → 走降级链"，
// 而那条路径根本不存在，会误导按它做设计的人）：
//  1. mock 模式 → mockProvider（不连外部模型，保证冒烟可离线跑）
//  2. llm.provider=mock → mockProvider（显式指定）
//  3. provider=rawhttp 及各 OpenAI 兼容厂商名 → rawHTTPProvider 单体
//  4. provider=eino 或留空 → 返回 error（einoprovider 未实现，见下）
//
// ★ 模型级降级（llm.models[].priority）不在本文件：rawhttp 实例持有
//
//	全部模型清单，换模型重试必须在 pkg/provider/rawhttp 内部做。
//	该能力目前尚未实现，配置里的 priority 只被读进能力清单，不参与路由。
//
// 返回的 cleanup 用于释放连接；调用方应在退出时调用。
func Build(ctx context.Context, c *conf.Config, mockMode bool) (provider.Provider, func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }

	// ---- 1) mock 模式 ----
	if mockMode {
		p := mockprovider.New(mockprovider.Options{
			Dim:        c.LLM.DefaultEmbedDim,
			ChatModel:  c.LLM.DefaultModel,
			EmbedModel: c.LLM.DefaultEmbedModel,
			// mock 流式分片间隔设为 0：冒烟脚本要快，
			// 流式逻辑本身仍被完整走一遍（真分片、末帧 usage）
			ChunkDelay: 0,
		})
		logger.Info(ctx, "provider: 使用 mock 实现",
			"chat_model", c.LLM.DefaultModel,
			"embed_model", c.LLM.DefaultEmbedModel,
			"dim", c.LLM.DefaultEmbedDim)
		return p, noop, nil
	}

	// ---- 2) 显式指定 mock ----
	if strings.EqualFold(c.LLM.Provider, "mock") {
		p := mockprovider.New(mockprovider.Options{
			Dim:        c.LLM.DefaultEmbedDim,
			ChatModel:  c.LLM.DefaultModel,
			EmbedModel: c.LLM.DefaultEmbedModel,
		})
		return p, noop, nil
	}

	switch strings.ToLower(c.LLM.Provider) {
	case "rawhttp", "raw-http", "http", "openai", "bailian", "dashscope", "aliyun", "deepseek", "qwen":
		// 所有 OpenAI 兼容厂商都走 rawHTTPProvider（标准 /v1/chat/completions 协议）
		p := rawhttp.New(rawhttp.Options{Conf: c.LLM})
		logger.Info(ctx, "provider: 使用 rawHTTP 薄客户端",
			"capabilities", p.Capabilities())
		return p, func(context.Context) error { return p.Close() }, nil

	case "eino", "":
		// einoProvider 尚未接入 CloudWeGo eino SDK。
		//
		// 当前降级策略：明确报错而不是静默返回 mock ——
		// 静默降级会让生产环境"看起来正常"但模型根本不生效，
		// 这类问题往往要到用户投诉才发现。
		//
		// 接入步骤见 docs/06-ADR.md ADR-003：
		//   1. go get github.com/cloudwego/eino-ext@<version>
		//   2. 在本文件实现 pkg/provider/einoprovider
		//   3. eino 支持 tool_call/reasoning，需如实声明 Capabilities
		return nil, noop, fmt.Errorf(
			"provider=eino 尚未实现；当前可选 rawhttp（能力子集）。" +
				"若需完整能力（ToolCall/Reasoning），请先实现 pkg/provider/einoprovider")

	default:
		return nil, noop, fmt.Errorf("未知的 provider: %q（可选 eino / rawhttp / mock）", c.LLM.Provider)
	}
}

// chainOf 已删除。
//
// 它原本用 chain 包把多个 Provider 串成降级链，但**没有任何调用方**，
// 而且机制与需求错配：pkg/provider/chain 解决的是"跨实现降级"
// （rawhttp ↔ eino ↔ mock），而配置里 llm.models[].priority 想要的是
// "跨模型降级"。一个 rawhttp Provider 实例本身就持有全部模型清单
// （Options{Conf: c.LLM}），所以 chainOf(rawhttp, rawhttp) 只会得到
// 同实现的两个副本，接上去也不会发生任何降级。
//
// 跨模型降级应在 rawhttp 内部按 priority 重试实现，不能靠这里包装。
// chain 包本身保留：它是上述跨实现降级的正确载体。
