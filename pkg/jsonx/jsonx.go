// Package jsonx 提供跨平台的 JSON 编解码入口。
//
// # 为什么需要这一层
//
// 项目初期选用 Go 1.26 的目的之一是能集成 Sonic 这类高性能组件。
// 但 Sonic 依赖 JIT 与平台相关指令集，并非任意环境都能跑：
//
//   - amd64 / arm64 且 Go 版本在支持区间内 → 走 JIT，性能最佳
//   - 其余组合（其他架构、未来的 Go 版本）→ Sonic 内部自动回落 encoding/json
//
// 也就是说，同一份代码在不同构建环境下**实际使用的引擎并不相同**。
// 若业务代码直接 import sonic，就会在跨平台构建时炸掉，
// 或者更糟：能编译通过、行为却悄悄变了。
//
// 本包把这层差异全部收敛：
//
//   - jsonx.Marshal / Unmarshal / ... 是业务唯一入口，签名固定
//   - 具体引擎由构建标签决定：-tags sonic 用 Sonic，否则用标准库
//   - Info() 报告当前进程真实使用的引擎，供启动自检打印
//
// # 用法
//
//	import "github.com/aegis/pkg/jsonx"
//
//	b, err := jsonx.Marshal(v)          // 无论哪个引擎，行为一致
//	fmt.Println(jsonx.Info().Summary())  // 启动时打一行，确认引擎
//
// # 构建
//
//	go build ./...                  # 标准库（默认，跨平台最稳）
//	go build -tags sonic ./...      # 启用 Sonic（仅支持平台才有加速）
//
// # ★ 行为一致性
//
// Sonic 与 encoding/json 存在若干默认行为差异，这些差异会让
// 「换一个引擎就换一个结果」，本包已在配置层显式对齐（见 sonicEngine 的注释）。
// 关键的三项：
//
//   - EscapeHTML：Sonic 默认**不转义** < > &，标准库默认转义。→ 对齐为转义
//   - SortMapKeys：标准库总是排序 map key，Sonic 默认不排。→ 对齐为排序
//   - nil slice/map：都输出 null。→ 天然一致
//
// 前两项若不对齐，同一份数据在两个引擎下会产出不同字节串。
// 对 JWT 这类要签名/校验的场景，这意味着签名在切换引擎后失效。
package jsonx

import (
	"io"
)

// EngineName 当前引擎名，取值 "sonic" 或 "encoding/json"
const (
	EngineSonic   = "sonic"
	EngineStdJSON = "encoding/json"
)

// EngineInfo 描述当前进程的 JSON 引擎状态。
//
// 之所以把它做成可查询的运行时信息，而不是一个编译期常量，
// 是因为「以为启用了 Sonic，实际在跑标准库」是最难排查的一类问题：
// 代码没报错、功能都对，只有性能悄悄退化了几个数量级。
// 启动时打印一行 Info()，这类问题在第一现场就能定位。
type EngineInfo struct {
	// Engine 实际引擎名（EngineSonic / EngineStdJSON）
	Engine string

	// Version 引擎版本：Sonic 为其模块版本，标准库为 Go 版本
	Version string

	// Native 当前平台是否被 Sonic 原生支持（amd64/arm64 + 受支持的 Go 版本）。
	//
	// 为 false 时即便用 -tags sonic 构建，Sonic 内部也会静默回落到标准库。
	Native bool

	// FallbackReason Native=false 时的原因说明，便于排查环境问题
	FallbackReason string

	// GOARCH / GOOS 构建目标，便于核对镜像是否构建在预期平台
	GOARCH string
	GOOS   string
}

// Summary 返回一行式摘要，用于日志输出。
//
// 形如：json engine=sonic version=v1.15.0 native=true goarch=amd64 goos=linux
// 或：   json engine=encoding/json version=go1.26.0 native=false reason="未使用 -tags sonic"
func (i EngineInfo) Summary() string {
	s := "json engine=" + i.Engine + " version=" + i.Version +
		" native=" + boolStr(i.Native) + " goarch=" + i.GOARCH + " goos=" + i.GOOS
	if !i.Native && i.FallbackReason != "" {
		s += " reason=" + quote(i.FallbackReason)
	}
	return s
}

// LogFields 返回可直接传给结构化日志的键值对。
func (i EngineInfo) LogFields() []any {
	f := []any{
		"json_engine", i.Engine,
		"json_version", i.Version,
		"json_native", i.Native,
	}
	if !i.Native && i.FallbackReason != "" {
		f = append(f, "json_fallback_reason", i.FallbackReason)
	}
	return f
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func quote(s string) string {
	// 原因文本里含空格（"未使用 -tags sonic"），不加引号日志会被切成多列
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(append(out, '"'))
}

// ---------- 跨引擎一致的公共门面 ----------
//
// 下面这些函数/类型不随构建标签变化。
// 具体转发到 stdEngine 或 sonicEngine，由同目录下的
// engine_std.go（无 sonic 标签）与 engine_sonic.go（有 sonic 标签）提供。

// Marshal 序列化为 JSON 字节数组
func Marshal(v any) ([]byte, error) { return marshal(v) }

// MarshalIndent 序列化为带缩进的 JSON
func MarshalIndent(v any, prefix, indent string) ([]byte, error) {
	return marshalIndent(v, prefix, indent)
}

// MarshalString 序列化为 JSON 字符串
func MarshalString(v any) (string, error) { return marshalString(v) }

// Unmarshal 反序列化 JSON 字节数组
func Unmarshal(data []byte, v any) error { return unmarshal(data, v) }

// UnmarshalString 反序列化 JSON 字符串
func UnmarshalString(s string, v any) error { return unmarshalString(s, v) }

// Valid 报告 JSON 是否合法
func Valid(data []byte) bool { return valid(data) }

// NewEncoder 创建流式编码器
func NewEncoder(w io.Writer) Encoder { return newEncoder(w) }

// NewDecoder 创建流式解码器
func NewDecoder(r io.Reader) Decoder { return newDecoder(r) }

// Encoder 流式编码器。
//
// 只暴露两个引擎都支持的最小方法集：
// Sonic 与标准库的 Encoder 方法集不同（标准库多 SetEscapeHTML），
// 这里只保留交集，调用方无法依赖某一个引擎独有的方法，
// 否则切换引擎时会出现编译通过、行为不同的问题。
type Encoder interface {
	Encode(v any) error
	SetIndent(prefix, indent string)
}

// Decoder 流式解码器
type Decoder interface {
	Decode(v any) error
	UseNumber()
	DisallowUnknownFields()
	Buffered() io.Reader
}
