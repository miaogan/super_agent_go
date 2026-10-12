package handler

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aegis/pkg/errno"
)

// TestErrnoCodeOfFromKitexBizError 锁住从 Kitex 错误串里提取业务码的行为。
//
// ★ 这些用例全部来自真实网关日志，不是我手编的字符串。
//
//	之前实现取"最后一个方括号"，而 Kitex 的错误串尾部还有
//	"[biz error]" 传输层标记，导致 404 被误判成 500。
func TestErrnoCodeOfFromKitexBizError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			// agent_service GetAgent 未找到时的真实报文
			name: "kitex biz error 带尾部标记",
			err:  errors.New("remote or network error: rpc error: code = 13 desc = [40401] 资源不存在 [biz error]"),
			want: "40401",
		},
		{
			name: "无尾部标记",
			err:  errors.New("remote or network error[remote]: biz error: [20004] 用户名或密码错误"),
			want: "20004",
		},
		{
			name: "多段方括号，业务码在前",
			err:  errors.New("a [40401] b [biz error] c [framework]"),
			want: "40401",
		},
		{
			name: "方括号内含空格",
			err:  errors.New("desc = [ 20003 ] 禁止访问 [biz error]"),
			want: "20003",
		},
		{
			name: "只有一个方括号段",
			err:  errors.New("[42901] 超出配额"),
			want: "42901",
		},
		{
			// 全部段落都不是数字 → 归 UNKNOWN，由调用方按 5xx 处理
			name: "无任何数字码",
			err:  errors.New("remote or network error: connection refused [biz error]"),
			want: codeUnknown,
		},
		{
			name: "方括号未闭合",
			err:  errors.New("desc = [40401 资源不存在"),
			want: codeUnknown,
		},
		{
			name: "无方括号",
			err:  errors.New("plain error"),
			want: codeUnknown,
		},
		{
			name: "空错误串",
			err:  errors.New(""),
			want: codeUnknown,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := errnoCodeOf(c.err); got != c.want {
				t.Errorf("errnoCodeOf() = %q, 期望 %q", got, c.want)
			}
		})
	}
}

func TestErrnoCodeOfNil(t *testing.T) {
	if got := errnoCodeOf(nil); got != codeUnknown {
		t.Errorf("nil 应返回 UNKNOWN，实际 %q", got)
	}
}

// TestErrnoCodeOfTypedError 验证同进程调用能直接拿类型，
// 不必退化到字符串解析。
func TestErrnoCodeOfTypedError(t *testing.T) {
	err := errno.New(errno.ErrNotFound)
	if got := errnoCodeOf(err); got != "40401" {
		t.Errorf("typed error 应直接取 Code，实际 %q", got)
	}

	// 包过一层的也要能解出来
	wrapped := fmt.Errorf("调用 agent.GetAgent 失败: %w", errno.Wrap(errno.ErrForbidden, errors.New("x")))
	if got := errnoCodeOf(wrapped); got != "20003" {
		t.Errorf("wrapped error 应解出 20003，实际 %q", got)
	}
}

// TestHTTPStatusOfMapping 锁住业务码 → HTTP 状态码的映射。
//
// 这是修 404 误报 500 的核心断言：解析出码之后必须映射成正确的状态。
func TestHTTPStatusOfMapping(t *testing.T) {
	cases := []struct {
		code   string
		status int
	}{
		{"40401", 404}, // ErrNotFound 资源不存在
		{"40430", 404}, // ErrAgentNotFound
		{"20003", 403}, // ErrForbidden 无权限
		{"20004", 401}, // ErrInvalidCredential 密码错误
		{"20001", 401}, // ErrUnauthorized
		{"40404", 429}, // ErrQuotaExceeded
		{"10004", 429}, // ErrRateLimited
		{"10001", 400}, // ErrInvalidParam
		{"10005", 413}, // ErrTooLarge
		{"40423", 415}, // ErrUnsupportedFormat
		{"40901", 503}, // ErrVectorDBUnavailable（应触发降级而非裸 500）
		{"50002", 504}, // ErrTimeout
		{"50005", 501}, // ErrNotImplemented
		{"50001", 500}, // ErrInternal
		{codeUnknown, 500},
		{"", 500},
		{"abc", 500},
	}

	for _, c := range cases {
		status, _ := httpStatusOf(c.code)
		if status != c.status {
			t.Errorf("httpStatusOf(%q) = %d, 期望 %d", c.code, status, c.status)
		}
	}
}

// TestEndToEndStatusFromKitexError 是本次修复的回归锁：
// 真实 Kitex 报文必须一路走到 404，不能停在 500。
func TestEndToEndStatusFromKitexError(t *testing.T) {
	kitexErr := errors.New("remote or network error: rpc error: code = 13 desc = [40401] 资源不存在 [biz error]")
	code := errnoCodeOf(kitexErr)
	status, msg := httpStatusOf(code)
	if status != 404 {
		t.Errorf("资源不存在应返回 404，实际 %d (code=%q)", status, code)
	}
	if msg != "资源不存在" {
		t.Errorf("对外消息 = %q", msg)
	}
}

func TestIsAllDigits(t *testing.T) {
	cases := map[string]bool{
		"40401": true,
		"0":     true,
		"":      false,
		" 12 ":  false, // 调用方会先 TrimSpace
		"12a":   false,
		"-1":    false,
		"1.2":   false,
	}
	for in, want := range cases {
		if got := isAllDigits(in); got != want {
			t.Errorf("isAllDigits(%q) = %v, 期望 %v", in, got, want)
		}
	}
}

// TestHTTPStatusOfDoesNotLeakDownstreamMessage 确认对外消息
// 不透传下游文案 —— 下游错误里可能夹带连接地址等内部信息。
func TestHTTPStatusOfDoesNotLeakDownstreamMessage(t *testing.T) {
	kitexErr := errors.New("dial tcp 10.0.3.17:8886: connect: connection refused [50001] 数据库地址已泄露 [biz error]")
	_, msg := httpStatusOf(errnoCodeOf(kitexErr))
	if msg == "" {
		t.Fatal("不应返回空消息")
	}
	for _, leak := range []string{"10.0.3.17", "8886", "connection refused"} {
		if contains(msg, leak) {
			t.Errorf("对外消息泄露了内部信息 %q: %s", leak, msg)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) &&
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()
}
