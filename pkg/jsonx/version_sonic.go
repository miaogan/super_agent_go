//go:build sonic

// 编译期注入的 sonic 版本号。
//
// 由构建系统通过 -ldflags "-X github.com/aegis/pkg/jsonx.sonicVer=vX.Y.Z" 写入。
// 未注入时为空字符串，sonicVersion() 会回落到 debug.ReadBuildInfo()。
//
// 单独成文件的原因：ldflags 的 -X 只能写字符串变量或指针，
// 而该值需要参与字符串拼接，必须是 string。
package jsonx

// sonicVer 由 -ldflags 注入，形如 "v1.15.0"
var sonicVer string
