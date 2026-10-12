package conf

import (
	"os"
	"testing"
)

// TestApplyEnvModelsByIndex 锁住「按索引覆盖模型列表」的行为。
//
// ★ 这项能力是被真实需求逼出来的：
//
//	llm.models 是结构体切片，每个模型有独立的 api_key / base_url / priority，
//	而applyEnv 一贯是显式逐字段声明（不支持切片元素）。
//	结果就是：容器里无法给6 个模型分别注入密钥 ——
//	写进配置文件等于把密钥烤进镜像层（docker history 可见），
//	不写则生产环境根本没有可用模型。
func TestApplyEnvModelsByIndex(t *testing.T) {
	c := Default()
	// ★ 注意：Default() 的 Models 刻意留空 ——
	//   模型清单属于部署配置（各环境厂商不同），不该硬编码进代码默认值，
	//   由配置文件或环境变量提供。
	//   所以这里自建列表，模拟「配置文件已加载 6 个模型」的真实前置条件。
	c.LLM.Models = []ModelConf{
		{Name: "chat-a", Priority: 10, MaxTokens: 1024},
		{Name: "chat-b", Priority: 20},
	}
	base := len(c.LLM.Models)

	c.LLM.Models = append(c.LLM.Models,
		ModelConf{Name: "chat-c", Priority: 30},
	)

	// 只给索引 base 的模型注入密钥与若干字段，其余保持原值
	t.Setenv("AEGIS_LLM__MODELS__"+itoa(base)+"__API_KEY", "sk-secret-abc")
	t.Setenv("AEGIS_LLM__MODELS__"+itoa(base)+"__BASE_URL", "https://gw.example.com/v1")
	t.Setenv("AEGIS_LLM__MODELS__"+itoa(base)+"__PRIORITY", "5")
	t.Setenv("AEGIS_LLM__MODELS__"+itoa(base)+"__MAX_TOKENS", "32768")
	t.Setenv("AEGIS_LLM__MODELS__"+itoa(base)+"__SUPPORTS_TOOL_CALL", "true")
	t.Setenv("AEGIS_LLM__MODELS__"+itoa(base)+"__TEMPERATURE", "0.25")

	// 索引 base+1 不设任何变量 —— 验证「未注入的不被改动」
	applyEnv(c)

	got := c.LLM.Models[base]
	if got.APIKey != "sk-secret-abc" {
		t.Errorf("API_KEY 未覆盖: %q", got.APIKey)
	}
	if got.BaseURL != "https://gw.example.com/v1" {
		t.Errorf("BASE_URL 未覆盖: %q", got.BaseURL)
	}
	if got.Priority != 5 {
		t.Errorf("PRIORITY 未覆盖: %d", got.Priority)
	}
	if got.MaxTokens != 32768 {
		t.Errorf("MAX_TOKENS 未覆盖: %d", got.MaxTokens)
	}
	if !got.SupportsToolCall {
		t.Error("SUPPORTS_TOOL_CALL 未覆盖")
	}
	if got.Temperature != 0.25 {
		t.Errorf("TEMPERATURE 未覆盖: %v", got.Temperature)
	}
	// 未显式声明的字段应保留原值（索引 base 的模型原名是 chat-c）
	if got.Name != "chat-c" {
		t.Errorf("NAME 被意外改动: %q", got.Name)
	}

	// 索引 0 完全未注入，密钥应仍为空
	if first := c.LLM.Models[0]; first.APIKey != "" {
		t.Errorf("未注入的索引不应被改动，APIKey=%q", first.APIKey)
	}
}

// TestApplyEnvModelSkipsBeyond 越界索引不应 panic。
//
// 运维常见场景：环境变量里留着 AEGIS_LLM__MODELS__9__API_KEY（配了 10 个模型），
// 而配置文件里只有 6 个 —— 此时必须静默忽略而不是崩在启动阶段。
func TestApplyEnvModelSkipsBeyond(t *testing.T) {
	c := Default()
	n := len(c.LLM.Models)

	os.Setenv("AEGIS_LLM__MODELS__"+itoa(n+10)+"__API_KEY", "sk-x")
	os.Setenv("AEGIS_LLM__MODELS__"+itoa(n+10)+"__PRIORITY", "1")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("越界索引导致 panic: %v", r)
		}
	}()

	applyEnv(c) // 不panic 即通过
}

// TestApplyEnvModelsInvalidValueIgnored 非法值应被静默忽略而非写入。
//
// 与既有 setInt / setFloat 的行为保持一致：
// 配错环境变量的正确后果是「用默认值继续跑」，不是「服务起不来」。
func TestApplyEnvModelsInvalidValueIgnored(t *testing.T) {
	c := Default()
	c.LLM.Models = []ModelConf{{Name: "chat-a", Priority: 7}}
	idx := itoa(0)
	origPriority := c.LLM.Models[0].Priority

	t.Setenv("AEGIS_LLM__MODELS__"+idx+"__PRIORITY", "not-a-number")

	applyEnv(c)

	if c.LLM.Models[0].Priority != origPriority {
		t.Errorf("非法值不应写入: 期望保持 %d，实际 %d",
			origPriority, c.LLM.Models[0].Priority)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
