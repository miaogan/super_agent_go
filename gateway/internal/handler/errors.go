package handler

import (
	"errors"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/aegis/pkg/errno"
)

// 错误码字符串化：Kitex 的 biz error 形如
//
//	remote or network error[remote]: biz error: [20004] 用户名或密码错误
//
// 我们不依赖 Kitex 的具体错误包装类型（跨版本会变），
// 而是用方括号里的数字码做匹配 —— 稳定且向前兼容。
const (
	codeUnknown = "UNKNOWN"
)

// errnoCodeOf 从下游错误中提取业务错误码。
//
// Kitex 会把 handler 返回的 error 包成 biz error，实测形如：
//
//	remote or network error: rpc error: code = 13 desc = [40401] 资源不存在 [biz error]
//
// ★ 关键：错误串里有多个方括号段落，且业务码在**前**、
//
//	"[biz error]" 这类传输层标记在**后**。
//	所以必须从左往右找第一个"纯数字"的方括号内容，
//	取最后一个会落到 "[biz error]"，解析失败后整个错误码退化成
//	UNKNOWN → 404 被误报成 500。
//
// 解析不出则归为 UNKNOWN，由调用方按 5xx 处理 ——
// 宁可报内部错误，也不要把未知错误伪装成客户端错误。
func errnoCodeOf(err error) string {
	if err == nil {
		return codeUnknown
	}
	// 同进程调用（mock / 单元测试）能直接拿到类型，不必走字符串解析
	var e *errno.Error
	if errors.As(err, &e) {
		return strconv.Itoa(int(e.Code))
	}

	msg := err.Error()
	for i := 0; i < len(msg); {
		open := strings.IndexByte(msg[i:], '[')
		if open < 0 {
			return codeUnknown
		}
		open += i
		close := strings.IndexByte(msg[open+1:], ']')
		if close < 0 {
			return codeUnknown
		}
		close += open + 1

		cand := strings.TrimSpace(msg[open+1 : close])
		if isAllDigits(cand) {
			return cand
		}
		i = close + 1 // 跳过 "[biz error]" 这类非数字段，继续往后找
	}
	return codeUnknown
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// httpStatusOf 由业务错误码决定 HTTP 状态码与对外消息。
//
// ★ 对外消息取 errno 的标准描述，不透传下游的自定义文案 ——
//
//	下游错误里可能夹带连接地址、配置片段等内部信息。
func httpStatusOf(code string) (status int, msg string) {
	if n, err := parseCode(code); err == nil {
		c := errno.Code(n)
		return c.HTTPStatus(), c.Message()
	}
	return consts.StatusInternalServerError, "服务内部错误"
}

// parseCode 解析错误码字符串
func parseCode(code string) (int, error) {
	return strconv.Atoi(code)
}
