//go:build !sonic

// 标准库实现。这是默认路径，也是所有非 amd64/arm64 平台的最终归宿。
//
// 无需任何构建标签即可编译，是本包的兜底保证：
// 即使 Sonic 在某环境下编译不过或性能不达标，
// 去掉 -tags sonic 就能得到一个纯标准库、零额外依赖的构建。
package jsonx

import (
	"encoding/json"
	"io"
	"runtime"
)

// stdEngineVersion 标准库"版本"用 Go 版本表示，
// 因为 encoding/json 没有独立版本号，它的兼容性由 go.mod 的 go 指令保证。
//
// 必须是变量而非常量：runtime.Version() 不是编译期常量。
var stdEngineVersion = runtime.Version()

// stdEncoder 把 *json.Encoder 适配为 Encoder 接口。
type stdEncoder struct{ e *json.Encoder }

func (s stdEncoder) Encode(v any) error { return s.e.Encode(v) }
func (s stdEncoder) SetIndent(prefix, indent string) {
	s.e.SetIndent(prefix, indent)
}

// stdDecoder 把 *json.Decoder 适配为 Decoder 接口。
type stdDecoder struct{ d *json.Decoder }

func (s stdDecoder) Decode(v any) error { return s.d.Decode(v) }
func (s stdDecoder) UseNumber()         { s.d.UseNumber() }
func (s stdDecoder) DisallowUnknownFields() {
	s.d.DisallowUnknownFields()
}
func (s stdDecoder) Buffered() io.Reader { return s.d.Buffered() }

func marshal(v any) ([]byte, error) { return json.Marshal(v) }

func marshalIndent(v any, prefix, indent string) ([]byte, error) {
	return json.MarshalIndent(v, prefix, indent)
}

func marshalString(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func unmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func valid(data []byte) bool { return json.Valid(data) }

func newEncoder(w io.Writer) Encoder { return stdEncoder{e: json.NewEncoder(w)} }

func newDecoder(r io.Reader) Decoder { return stdDecoder{d: json.NewDecoder(r)} }

func engineInfo() EngineInfo {
	reason := "构建未启用 -tags sonic"
	if NativeSupported() {
		// 当前平台本可以跑 Sonic
		reason = "当前平台支持 sonic，但构建未启用 -tags sonic"
	}
	return EngineInfo{
		Engine:         EngineStdJSON,
		Version:        stdEngineVersion,
		Native:         false,
		FallbackReason: reason,
		GOARCH:         runtime.GOARCH,
		GOOS:           runtime.GOOS,
	}
}
