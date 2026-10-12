package errno

import (
	"errors"
	"fmt"
	"testing"
)

// TestHTTPStatus 错误码 → HTTP 状态码映射，覆盖代表性错误码。
func TestHTTPStatus(t *testing.T) {
	cases := []struct {
		code Code
		want int
	}{
		{OK, 200},
		{ErrInvalidParam, 400},
		{ErrTooLarge, 413},
		{ErrRateLimited, 429},
		{ErrUnauthorized, 401},
		{ErrForbidden, 403},
		{ErrOAuthExchangeFailed, 502},
		{ErrNotFound, 404},
		{ErrAlreadyExists, 409},
		{ErrKBAccessDenied, 403},
		{ErrUnsupportedFormat, 415},
		{ErrDocumentTooLarge, 413},
		{ErrParseFailed, 422},
		{ErrIngestBacklog, 429},
		{ErrVectorDBUnavailable, 503},
		{ErrInternal, 500},
		{ErrTimeout, 504},
		{ErrNotImplemented, 501},
		{ErrStorageFull, 507},
	}
	for _, c := range cases {
		if got := c.code.HTTPStatus(); got != c.want {
			t.Errorf("HTTPStatus(%d) = %d, 期望 %d", c.code, got, c.want)
		}
	}
	// 未注册的错误码回落到 500
	if got := Code(99999).HTTPStatus(); got != 500 {
		t.Errorf("未注册错误码应回落 500，实际 %d", got)
	}
}

// TestMessage 已知错误码应有中文描述，未知错误码返回兜底文案。
func TestMessage(t *testing.T) {
	if got := ErrForbidden.Message(); got == "" || got == "未知错误" {
		t.Errorf("ErrForbidden.Message() 应返回具体描述，实际 %q", got)
	}
	if got := Code(99999).Message(); got != "未知错误" {
		t.Errorf("未知错误码应返回兜底文案，实际 %q", got)
	}
	if got := OK.Message(); got == "" {
		t.Error("OK 也应有描述")
	}
}

// TestErrorString Error 的字符串化：自定义 Msg 优先，否则用默认描述。
func TestErrorString(t *testing.T) {
	e := Newf(ErrInvalidParam, "字段 %s 非法", "page")
	if got := e.Error(); got != "[10001] 字段 page 非法" {
		t.Errorf("带自定义 Msg 的 Error 字符串不符合预期: %q", got)
	}
	e2 := New(ErrForbidden)
	if got := e2.Error(); got != "[20003] 无权限执行该操作" {
		t.Errorf("默认描述的 Error 字符串不符合预期: %q", got)
	}
}

// TestWrapAndUnwrap Wrap 保留 Cause 且 Unwrap 可还原。
func TestWrapAndUnwrap(t *testing.T) {
	cause := errors.New("底层原因")
	e := Wrap(ErrUpstreamError, cause)
	if e.Code != ErrUpstreamError {
		t.Errorf("Wrap 应保留错误码: %v", e.Code)
	}
	if !errors.Is(e, cause) {
		t.Error("Wrap 后 errors.Is 应能匹配 Cause")
	}
	if errors.Unwrap(e) != cause {
		t.Error("Unwrap 应返回 Cause")
	}
	// Wrapf
	e2 := Wrapf(ErrTimeout, cause, "调用 %s 超时", "llm")
	if e2.Msg != "调用 llm 超时" || errors.Unwrap(e2) != cause {
		t.Errorf("Wrapf 结果不符: %+v", e2)
	}
}

// TestCodeOf 提取业务错误码：业务错误返回其码，nil 返回 OK，非业务返回 ErrInternal。
func TestCodeOf(t *testing.T) {
	if got := CodeOf(nil); got != OK {
		t.Errorf("CodeOf(nil) = %v, 期望 OK", got)
	}
	if got := CodeOf(New(ErrKBNotFound)); got != ErrKBNotFound {
		t.Errorf("CodeOf 应提取错误码, 实际 %v", got)
	}
	if got := CodeOf(Wrap(ErrModelUnavailable, errors.New("x"))); got != ErrModelUnavailable {
		t.Errorf("CodeOf 应穿透 Wrap, 实际 %v", got)
	}
	if got := CodeOf(errors.New("普通错误")); got != ErrInternal {
		t.Errorf("CodeOf(普通错误) = %v, 期望 ErrInternal", got)
	}
}

// TestMessageOf 提取描述：自定义 Msg 优先，其次默认描述，最后底层 error 字符串。
func TestMessageOf(t *testing.T) {
	if got := MessageOf(nil); got != "" {
		t.Errorf("MessageOf(nil) 应为空, 实际 %q", got)
	}
	if got := MessageOf(Newf(ErrInvalidParam, "自定义")); got != "自定义" {
		t.Errorf("MessageOf 应优先自定义 Msg: %q", got)
	}
	if got := MessageOf(New(ErrKBNotFound)); got != "知识库不存在" {
		t.Errorf("MessageOf 应回落默认描述: %q", got)
	}
	if got := MessageOf(errors.New("raw")); got != "raw" {
		t.Errorf("MessageOf(普通错误) 应返回其字符串: %q", got)
	}
}

// TestErrorChainAs 嵌套错误仍可通过 errors.As 提取 *Error。
func TestErrorChainAs(t *testing.T) {
	err := fmtWrap(New(ErrConflict))
	var e *Error
	if !errors.As(err, &e) {
		t.Fatal("errors.As 应能从嵌套链提取 *Error")
	}
	if e.Code != ErrConflict {
		t.Errorf("提取出的错误码不符: %v", e.Code)
	}
}

// fmtWrap 用 fmt.Errorf %w 再包一层，模拟跨服务错误包装。
func fmtWrap(err error) error {
	return fmt.Errorf("outer: %w", err)
}
