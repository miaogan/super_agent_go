//go:build sonic

// Sonic 实现。仅在显式传入 -tags sonic 时编译。
//
// # 为什么用 ConfigStd 而不是 ConfigDefault
//
// Sonic 自带两套预置配置，注释写得很清楚：
//
//	ConfigDefault —— "aiming at efficiency and safety"（快，但与标准库有行为差异）
//	ConfigStd     —— "aiming to be compatible with encoding/json"
//
// 差异是实打实的，不是理论问题：
//
//   - EscapeHTML：ConfigDefault 为 false，< > & 不会转义成 <；
//     标准库 json.Marshal 一定转义。同一段含 <script> 的文本，
//     两个引擎产出的字节串不同。
//   - SortMapKeys：ConfigDefault 为 false，map key 按插入顺序；
//     标准库始终按 key 排序。Go 的 map 遍历顺序本来就是随机的，
//     于是同一份数据两次序列化可能得到不同字节 —— 用于签名或做缓存键都会出问题。
//
// 对本项目而言第一项尤其关键：JWT 的 header/claims 要 base64url 编码后参与签名，
// 字节不一致就意味着「A 服务签发的令牌在 B 服务验不过」。
// 所以这里显式选 ConfigStd，用 Sonic 的性能换 encoding/json 的行为。
//
// # 非原生平台
//
// Sonic 自身的 compat.go 带了构建标签，在不支持的平台上会自动回落 encoding/json。
// 也就是说 -tags sonic 在任何平台都能编译通过，只是没有加速。
// 本文件通过 APIKind 探测实际走向，让 Info() 如实报告，而不是假装在用 Sonic。
package jsonx

import (
	"io"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/bytedance/sonic"
)

// api 为对齐标准库行为而冻结的 API 实例
var api = sonic.ConfigStd

// sonicEncoder 把 sonic.Encoder 适配为 Encoder 接口
type sonicEncoder struct{ e sonic.Encoder }

func (s sonicEncoder) Encode(v any) error { return s.e.Encode(v) }
func (s sonicEncoder) SetIndent(prefix, indent string) {
	s.e.SetIndent(prefix, indent)
}

// sonicDecoder 把 sonic.Decoder 适配为 Decoder 接口
type sonicDecoder struct{ d sonic.Decoder }

func (s sonicDecoder) Decode(v any) error { return s.d.Decode(v) }
func (s sonicDecoder) UseNumber()         { s.d.UseNumber() }
func (s sonicDecoder) DisallowUnknownFields() {
	s.d.DisallowUnknownFields()
}
func (s sonicDecoder) Buffered() io.Reader { return s.d.Buffered() }

func marshal(v any) ([]byte, error) { return api.Marshal(v) }

func marshalIndent(v any, prefix, indent string) ([]byte, error) {
	return api.MarshalIndent(v, prefix, indent)
}

func marshalString(v any) (string, error) { return api.MarshalToString(v) }

func unmarshal(data []byte, v any) error { return api.Unmarshal(data, v) }

func unmarshalString(s string, v any) error { return api.UnmarshalFromString(s, v) }

func valid(data []byte) bool { return api.Valid(data) }

func newEncoder(w io.Writer) Encoder { return sonicEncoder{e: api.NewEncoder(w)} }

func newDecoder(r io.Reader) Decoder { return sonicDecoder{d: api.NewDecoder(r)} }

func engineInfo() EngineInfo {
	info := EngineInfo{
		GOARCH: runtime.GOARCH,
		GOOS:   runtime.GOOS,
	}

	// ★ APIKind 是 Sonic 自己在两条实现里各自定义的常量：
	//   原生实现（api.go）  → UseSonicJSON
	//   回落实现（compat.go）→ UseStdJSON
	// 编译进来的必然是其中之一，所以这里能准确判断「有没有真的在跑 JIT」。
	// 只看构建标签是不够的：-tags sonic 在 arm64 上一样能编过，
	// 但可能因为 Go 版本不受支持而回落到标准库。
	if sonic.APIKind == sonic.UseStdJSON {
		info.Engine = EngineStdJSON
		info.Version = runtime.Version()
		info.Native = false
		info.FallbackReason = fallbackReason()
		return info
	}

	info.Engine = EngineSonic
	info.Version = sonicVersion()
	info.Native = true
	return info
}

func fallbackReason() string {
	if !NativeSupported() {
		return "当前平台 " + runtime.GOARCH + " 不受 sonic 支持（仅 amd64/arm64）"
	}
	return "当前 Go 版本 " + runtime.Version() + " 不在 sonic 的 JIT 支持区间内，已回落标准库"
}

// sonicVersion 尽力解析 sonic 的模块版本。
//
// Sonic 没有导出 Version 常量，只能从构建信息里读。两种来源：
//
//  1. debug.ReadBuildInfo() —— 二进制构建时写入 deps，正常可用
//  2. 编译期常量 sonicVer —— 由 make / scripts/gen_version.sh 注入
//
// 为什么必须有第2 条：go test 生成的测试二进制不写 deps，
// 于是测试里会读到 "unknown"。这不算大碍，但会让
// 「sonic 到底装了哪个版本」在排障时缺一块关键信息。
var sonicVersion = detectSonicVersion

// detectSonicVersion 实际探测逻辑
func detectSonicVersion() string {
	// ① 构建期注入的最高优先级
	if v := strings.TrimSpace(sonicVer); v != "" {
		return v
	}
	// ② 从二进制构建信息读
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	// 自身即主模块时（如 go run）版本在 bi.Main
	if strings.HasPrefix(bi.Main.Path, "github.com/bytedance/sonic") {
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			return bi.Main.Version
		}
	}
	for _, dep := range bi.Deps {
		if dep == nil || !strings.HasPrefix(dep.Path, "github.com/bytedance/sonic") {
			continue
		}
		if dep.Replace != nil {
			// 被 replace 过：vX.Y.Z=>path 或 vX.Y.Z=>path vA.B.C
			if dep.Replace.Version != "" {
				return dep.Replace.Version + "=>" + dep.Replace.Path
			}
			return dep.Replace.Path
		}
		if dep.Version != "" {
			return dep.Version
		}
	}
	return "unknown"
}
