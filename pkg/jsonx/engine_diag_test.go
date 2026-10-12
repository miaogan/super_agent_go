package jsonx

import (
	"encoding/json"
	"testing"
)

// TestPrintEngineInfo 把当前引擎信息打到测试输出里。
//
// 用途是给运维一个零成本的诊断手段：
//
//	make engine-info          # 两种构建各跑一次
//	go test ./pkg/jsonx/ -run TestPrintEngineInfo -v
//
// 「服务能跑但性能不如预期」这类问题的第一排查动作，
// 就是确认跑在哪个 JSON 引擎上 —— 这个测试让该动作只需要一条命令。
func TestPrintEngineInfo(t *testing.T) {
	info := Info()
	t.Logf("json %s", info.Summary())

	// 同时校验自检信息本身可信，避免「诊断工具自己撒谎」
	if info.Engine == EngineSonic && !info.Native {
		t.Error("引擎标为 sonic 但 Native=false")
	}
	if info.Native && info.Engine != EngineSonic {
		t.Error("Native=true 但引擎不是 sonic")
	}
}

// TestEngineSmokeSmoke 是最低限度的引擎可用性验证。
//
// 与其他测试不同，它刻意只依赖 jsonx 自身的 API，
// 用于回答「这个引擎在本平台上到底能不能正常工作」。
func TestEngineSmokeSmoke(t *testing.T) {
	type payload struct {
		A int               `json:"a"`
		B string            `json:"b"`
		C map[string]string `json:"c"`
	}
	in := payload{A: 42, B: "sonic", C: map[string]string{"k": "v"}}

	b, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	var out payload
	if err := Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if out.A != in.A || out.B != in.B || out.C["k"] != "v" {
		t.Errorf("往返不一致: %+v", out)
	}

	// 再用标准库交叉读一次，确认产出的确实是合法 JSON
	var cross map[string]any
	if err := json.Unmarshal(b, &cross); err != nil {
		t.Errorf("本引擎输出无法被标准库解析: %v", err)
	}
}
