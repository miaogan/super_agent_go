package conf

import (
	"os"
	"strings"
	"testing"
)

// TestPrintModelConfig 供 `make model-info` 调用：
// 加载真实配置文件并打印模型清单，便于确认「实际生效的是哪些模型」。
//
// ★ 为什么需要它：
//
//	模型配置有3 个来源（yaml / 环境变量 / 未来的 Nacos），
//	改错时最费时间的是「到底哪一份生效了」。
//	启动日志只说 provider=rawhttp，不说模型清单 —— 这个入口补上这块。
//
// 注意：密钥一律脱敏打印，避免日志泄露。
func TestPrintModelConfig(t *testing.T) {
	path := os.Getenv("AEGIS_CONF_FILE")
	if path == "" {
		path = "../../configs/config.example.yaml"
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("加载配置失败 %s: %v", path, err)
	}
	t.Logf("配置文件: %s", path)
	t.Logf("llm provider=%s default_model=%s default_embed=%s dim=%d",
		c.LLM.Provider, c.LLM.DefaultModel, c.LLM.DefaultEmbedModel, c.LLM.DefaultEmbedDim)
	t.Logf("模型清单(%d 个):", len(c.LLM.Models))
	for i, m := range c.LLM.Models {
		key := m.APIKey
		if key != "" {
			// 只显示前4 位，足以判断"配没配上"，不足以泄露
			if len(key) > 4 {
				key = key[:4] + "***"
			} else {
				key = "***"
			}
		} else {
			key = "(未配置)"
		}
		t.Logf("  [%d] %-30s prio=%-3d status=%-9s key=%-12s tool_call=%-5v vision=%-5v reasoning=%v",
			i, m.Name, m.Priority, m.Status, key,
			m.SupportsToolCall, m.SupportsVision, m.SupportsReasoning)
	}
	t.Logf("milvus dim=%d metric=%s collection=%s", c.Milvus.Dim, c.Milvus.MetricType, c.Milvus.Collection)

	// 一致性检查：维度不匹配是 RAG 最难排查的故障
	if c.LLM.DefaultEmbedDim != c.Milvus.Dim {
		t.Errorf("维度不一致: llm.default_embed_dim=%d 与 milvus.dim=%d 不符，"+
			"写入与检索都会失败", c.LLM.DefaultEmbedDim, c.Milvus.Dim)
	}
}

// TestLoadExampleConfig 确认示例配置本身合法且自洽。
//
// 它同时是「文档不会腐烂」的保障：README 里贴的配置片段一旦与结构体脱节，
// 这个用例会立刻失败。
func TestLoadExampleConfig(t *testing.T) {
	c, err := Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatalf("config.example.yaml 无法加载: %v", err)
	}

	if c.LLM.Provider == "" {
		t.Error("llm.provider 为空")
	}
	if c.LLM.DefaultEmbedDim == 0 {
		t.Error("llm.default_embed_dim 为 0")
	}
	if len(c.LLM.Models) == 0 {
		t.Fatal("llm.models 为空：示例配置应至少列出模型")
	}

	// 同一优先级内不应出现重名（降级链会按列表顺序尝试，重名难排查）
	seen := map[string]int{}
	for i, m := range c.LLM.Models {
		if m.Name == "" {
			t.Errorf("[%d] 模型名为空", i)
			continue
		}
		if m.BaseURL == "" {
			t.Errorf("[%d] %s 的 base_url 为空", i, m.Name)
		}
		if prev, dup := seen[m.Name]; dup {
			t.Errorf("模型名重复: %s（索引 %d 与 %d）", m.Name, prev, i)
		}
		seen[m.Name] = i
	}

	// 默认对话模型必须真实存在于清单里，否则运行期才发现就晚了
	found := false
	for _, m := range c.LLM.Models {
		if m.Name == c.LLM.DefaultModel {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("llm.default_model=%q 不在 models 清单中", c.LLM.DefaultModel)
	}

	// 工具出站策略：yaml key 拼错时不会报错、不会告警，只是静默不生效
	// （本项目已因此在 configs 上踩过两次），所以逐个断言真的绑上了。
	if len(c.Agent.ToolAllowHosts) == 0 {

		t.Error("agent.tool_allow_hosts 未绑定：示例配置应给出联调用主机名单")
	}
	bound := false
	for _, h := range c.Agent.ToolAllowHosts {
		if h == "host.docker.internal" {
			bound = true
		}
	}
	if !bound {
		t.Errorf("agent.tool_allow_hosts 内容不符: %v", c.Agent.ToolAllowHosts)
	}
	if c.Agent.ToolAllowPrivateIPs {
		t.Error("示例配置不应默认放开内网：agent.tool_allow_private_ips 必须为 false")
	}
	if c.Agent.ToolFetchTimeout <= 0 {
		t.Errorf("agent.tool_fetch_timeout 未绑定或非法: %v", c.Agent.ToolFetchTimeout)
	}
	if c.Agent.ToolMaxResponseBytes <= 0 {
		t.Errorf("agent.tool_max_response_bytes 未绑定或非法: %d", c.Agent.ToolMaxResponseBytes)
	}

	// 维度自洽
	if c.LLM.DefaultEmbedDim != c.Milvus.Dim {
		t.Errorf("维度不一致: llm.default_embed_dim=%d, milvus.dim=%d",
			c.LLM.DefaultEmbedDim, c.Milvus.Dim)
	}

	// PDF 引擎与格式白名单同样逐个断言绑定：
	// allowed_types 里出现 pkg/parser 不支持的格式，文件能上传但必然解析失败；
	// mineru.timeout 拼错则回落到代码默认值，运维改配置却看不到效果。
	for _, want := range []string{"pdf", "docx", "pptx", "xlsx", "html", "csv", "md", "txt"} {
		found := false
		for _, t := range c.RAG.AllowedTypes {
			if strings.EqualFold(t, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("rag.allowed_types 缺少 %q（示例配置应与 parser 支持的格式一致）", want)
		}
	}
	if c.RAG.PDFParser != "local" {
		t.Errorf(`rag.pdf_parser 应为 "local"，实际 %q`, c.RAG.PDFParser)
	}
	if c.RAG.MinerU.Timeout <= 0 {
		t.Errorf("rag.mineru.timeout 未绑定: %v", c.RAG.MinerU.Timeout)
	}
	if c.RAG.MinerU.MaxResponseMB <= 0 {
		t.Errorf("rag.mineru.max_response_mb 未绑定: %d", c.RAG.MinerU.MaxResponseMB)
	}
	// 示例配置选 local 时 base_url 必须为空；两者同时出现会让用户照抄到
	// 一个"配了但没启用"的矛盾状态。
	if c.RAG.PDFParser == "local" && c.RAG.MinerU.BaseURL != "" {
		t.Error("pdf_parser=local 时示例配置不应给出 mineru.base_url，避免误导")
	}
}
