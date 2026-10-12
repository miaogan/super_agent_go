//go:build !((amd64 || arm64) && !go1.27 && go1.17 && (!arm64 || go1.20))

// 不原生支持 Sonic：走 compat.go 的 encoding/json 回落实现。
//
// 具体情形：
//   - 非 amd64/arm64 架构（如 riscv64、ppc64le、wasm）
//   - Go 1.27 及以上（Sonic 尚未适配，会回落）
//   - Go 1.17 以下
//   - arm64 且 Go 1.20 以下
//
// 注意：即便用 -tags sonic 构建，本文件仍然生效 —— Sonic 包内部
// 同样会走 compat.go。此时 Info() 报告 Engine="encoding/json"，
// 而不是假装在用 Sonic。
package jsonx

const nativeSupportedValue = false
