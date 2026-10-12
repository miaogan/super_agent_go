// 平台原生性探测。此文件在标准库与 Sonic 两种构建下都编译。
//
// # 判定依据
//
// Sonic 的 compat.go 使用如下构建标签（原文以旧式注释书写，此处为可读性去掉前缀）：
//
//	!amd64,!arm64 go1.27 !go1.17 arm64,!go1.20
//
// 逗号是「且」、空格是「或」，展开后命中任一条即走兼容实现：
//
//	1. 非 amd64 且非 arm64
//	2. Go 1.27 及以上
//	3. Go 1.17 以下
//	4. arm64 且 Go 1.20 以下
//
// 因此「原生支持」= 架构是 amd64 或 arm64，且
// (amd64 时 1.17 ≤ Go < 1.27；arm64 时 1.20 ≤ Go < 1.27)。
//
// # 为什么也用构建标签而不是解析版本号
//
// 本文件采用与 Sonic 完全相同的机制（go1.x 构建标签）判定，
// 而不是在运行时解析 runtime.Version() 的字符串。理由：
//
//   - 构建标签由工具链判定，与 Sonic 自身用的是同一套语义，
//     不会出现「我们说支持、Sonic 说回落」的双标
//   - devel 版本（如 go1.27-devel-xxxx）用字符串解析极易误判，
//     构建标签没有这个歧义
//
// 上游若调整支持区间，本包需要同步更新 platform_native.go 的条件；
// 但由于 Info() 另外会用 sonic.APIKind 校验「实际走的是哪条实现」，
// 即使条件写宽了也只是多报告一句可用，不会把标准库误报成 Sonic。

package jsonx

// NativeSupported 判断当前构建目标是否被 Sonic 原生支持。
//
// 无论是否用 -tags sonic 构建都可调用：
// 标准库实现用它来回答「这台机器本来能不能跑 Sonic」。
func NativeSupported() bool { return nativeSupported }

// nativeSupported 由 platform_native.go / platform_fallback.go 二选一提供。
const nativeSupported = nativeSupportedValue

// Info 返回当前 JSON 引擎信息，用于启动自检。
//
// 形如：json engine=sonic version=v1.15.0 native=true goarch=amd64 goos=linux
// 或：  json engine=encoding/json version=go1.26.0 native=false reason="..."
//
// engineInfo 由 engine_std.go / engine_sonic.go 二选一提供，
// 故本函数在两种构建下均可用。
func Info() EngineInfo { return engineInfo() }
