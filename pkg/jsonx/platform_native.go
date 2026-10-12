//go:build (amd64 || arm64) && !go1.27 && go1.17 && (!arm64 || go1.20)

// 原生支持 Sonic：amd64/arm64 且 Go 版本落在 JIT 支持区间内。
//
// 条件与 Sonic compat.go 的标签严格互补：
//
//	compat: !amd64,!arm64  或  go1.27  或  !go1.17  或  (arm64 且 !go1.20)
//	native: (amd64 或 arm64) 且 !go1.27 且 go1.17 且 !(arm64 且 !go1.20)
//
// arm64 单独要求 go1.20，是因为 sonic 的 JIT 编码器在 arm64 上
// 依赖的指令在 Go 1.20 之前不可用。
package jsonx

const nativeSupportedValue = true
