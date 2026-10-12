// Package errno 定义全系统统一错误码。
//
// 设计约定（与 docs/03-API接口文档.md 的错误码表一致）：
//   - 1xxxx 通用/参数类
//   - 2xxxx 认证授权类
//   - 4xxxx 业务资源类
//   - 5xxxx 系统/依赖类
//
// Kitex 侧通过 bizerr 包装为 *kitex.BizError，gateway 解析后映射为 HTTP 状态码。
package errno

import "fmt"

// Code 业务错误码
type Code int32

const (
	// ---------- 成功 ----------
	OK Code = 0

	// ---------- 1xxxx 通用 ----------
	ErrInvalidParam  Code = 10001 // 参数非法
	ErrMissingParam  Code = 10002 // 缺少必填参数
	ErrInvalidFormat Code = 10003 // 格式错误
	ErrRateLimited   Code = 10004 // 触发限流
	ErrTooLarge      Code = 10005 // 请求体过大

	// ---------- 2xxxx 认证授权 ----------
	ErrUnauthorized      Code = 20001 // 未认证（无 token 或 token 无效）
	ErrTokenExpired      Code = 20002 // token 已过期
	ErrForbidden         Code = 20003 // 无权限
	ErrInvalidCredential Code = 20004 // 用户名或密码错误
	ErrAccountDisabled   Code = 20005 // 账号被禁用
	ErrCaptchaRequired   Code = 20006 // 需要验证码

	// OAuth2.0 相关
	ErrOAuthStateMismatch  Code = 20010 // state 校验失败（可能 CSRF）
	ErrOAuthExchangeFailed Code = 20011 // 用 code 换 token 失败
	ErrIdPNotFound         Code = 20012 // IdP 未配置或未启用
	ErrIdPUnreachable      Code = 20013 // IdP 不可达

	// ---------- 4xxxx 业务资源 ----------
	ErrNotFound      Code = 40401 // 资源不存在
	ErrAlreadyExists Code = 40402 // 资源已存在
	ErrConflict      Code = 40403 // 状态冲突
	ErrQuotaExceeded Code = 40404 // 配额超限

	// 用户
	ErrUserNotFound  Code = 40410 // 用户不存在
	ErrUsernameTaken Code = 40411 // 用户名已占用

	// 知识库 / RAG
	ErrKBNotFound           Code = 40420 // 知识库不存在
	ErrDocNotFound          Code = 40421 // 文档不存在
	ErrKBAccessDenied       Code = 40422 // 知识库越权访问
	ErrUnsupportedFormat    Code = 40423 // 不支持的文件格式
	ErrDocumentTooLarge     Code = 40424 // 文档过大
	ErrParseFailed          Code = 40425 // 文档解析失败
	ErrEmbeddingDimMismatch Code = 40426 // 向量维度不匹配
	ErrIngestBacklog        Code = 40427 // 向量化任务积压

	// 会话 / Agent
	ErrAgentNotFound   Code = 40430
	ErrSessionNotFound Code = 40431
	ErrAgentDisabled   Code = 40432

	// 向量检索
	ErrVectorDBUnavailable Code = 40901 // Milvus 不可用（应走降级）
	ErrVectorSearchFailed  Code = 40902

	// ---------- 5xxxx 系统 ----------
	ErrInternal         Code = 50001 // 内部错误
	ErrTimeout          Code = 50002 // 超时
	ErrModelUnavailable Code = 50003 // 模型服务不可用
	ErrUpstreamError    Code = 50004 // 上游依赖错误
	ErrNotImplemented   Code = 50005 // 未实现
	ErrStorageFull      Code = 50701 // 存储空间不足
)

// httpStatus 错误码到 HTTP 状态码的映射，供 gateway 使用
var httpStatus = map[Code]int{
	OK: 200,

	ErrInvalidParam: 400, ErrMissingParam: 400, ErrInvalidFormat: 400,
	ErrTooLarge: 413, ErrRateLimited: 429,

	ErrUnauthorized: 401, ErrTokenExpired: 401, ErrInvalidCredential: 401,
	ErrCaptchaRequired: 401,
	ErrForbidden:       403, ErrAccountDisabled: 403,
	ErrOAuthStateMismatch: 400, ErrOAuthExchangeFailed: 502,
	ErrIdPNotFound: 404, ErrIdPUnreachable: 502,

	ErrNotFound: 404, ErrAlreadyExists: 409, ErrConflict: 409, ErrQuotaExceeded: 429,
	ErrUserNotFound: 404, ErrUsernameTaken: 409,
	ErrKBNotFound: 404, ErrDocNotFound: 404, ErrKBAccessDenied: 403,
	ErrUnsupportedFormat: 415, ErrDocumentTooLarge: 413,
	ErrParseFailed: 422, ErrEmbeddingDimMismatch: 422, ErrIngestBacklog: 429,
	ErrAgentNotFound: 404, ErrSessionNotFound: 404, ErrAgentDisabled: 403,
	ErrVectorDBUnavailable: 503, ErrVectorSearchFailed: 500,

	ErrInternal: 500, ErrTimeout: 504, ErrModelUnavailable: 503,
	ErrUpstreamError: 502, ErrNotImplemented: 501, ErrStorageFull: 507,
}

// HTTPStatus 返回错误码对应的 HTTP 状态码，未定义时回落到 500。
func (c Code) HTTPStatus() int {
	if s, ok := httpStatus[c]; ok {
		return s
	}
	return 500
}

// Message 返回错误码的默认中文描述。
func (c Code) Message() string {
	if m, ok := messages[c]; ok {
		return m
	}
	return "未知错误"
}

var messages = map[Code]string{
	OK: "成功",

	ErrInvalidParam:  "参数非法",
	ErrMissingParam:  "缺少必填参数",
	ErrInvalidFormat: "格式错误",
	ErrRateLimited:   "请求过于频繁，请稍后重试",
	ErrTooLarge:      "请求体过大",

	ErrUnauthorized:      "未认证或凭证无效",
	ErrTokenExpired:      "登录已过期，请重新登录",
	ErrForbidden:         "无权限执行该操作",
	ErrInvalidCredential: "用户名或密码错误",
	ErrAccountDisabled:   "账号已被禁用",
	ErrCaptchaRequired:   "请先完成验证码校验",

	ErrOAuthStateMismatch:  "登录状态校验失败，请重新发起登录",
	ErrOAuthExchangeFailed: "第三方登录凭证换取失败",
	ErrIdPNotFound:         "未找到可用的身份提供方",
	ErrIdPUnreachable:      "身份提供方暂时不可达",

	ErrNotFound:      "资源不存在",
	ErrAlreadyExists: "资源已存在",
	ErrConflict:      "当前状态不允许该操作",
	ErrQuotaExceeded: "配额已超限",

	ErrUserNotFound:  "用户不存在",
	ErrUsernameTaken: "用户名已被占用",

	ErrKBNotFound:           "知识库不存在",
	ErrDocNotFound:          "文档不存在",
	ErrKBAccessDenied:       "无权访问该知识库",
	ErrUnsupportedFormat:    "不支持的文件格式",
	ErrDocumentTooLarge:     "文档超出大小限制",
	ErrParseFailed:          "文档解析失败",
	ErrEmbeddingDimMismatch: "向量维度与知识库配置不匹配",
	ErrIngestBacklog:        "向量化任务积压，请稍后重试",

	ErrAgentNotFound:   "智能体不存在",
	ErrSessionNotFound: "会话不存在",
	ErrAgentDisabled:   "智能体已停用",

	ErrVectorDBUnavailable: "向量检索服务暂时不可用",
	ErrVectorSearchFailed:  "向量检索失败",

	ErrInternal:         "服务内部错误",
	ErrTimeout:          "请求超时",
	ErrModelUnavailable: "模型服务暂时不可用",
	ErrUpstreamError:    "上游依赖调用失败",
	ErrNotImplemented:   "功能尚未实现",
	ErrStorageFull:      "存储空间不足",
}

// Error 实现 error 接口，便于直接作为 error 返回。
type Error struct {
	Code Code
	Msg  string
	// Cause 保存原始错误，不对外暴露，仅用于日志
	Cause error
}

func (e *Error) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("[%d] %s", e.Code, e.Msg)
	}
	return fmt.Sprintf("[%d] %s", e.Code, e.Code.Message())
}

func (e *Error) Unwrap() error { return e.Cause }

// New 用错误码构造 error，使用默认描述。
func New(c Code) *Error {
	return &Error{Code: c}
}

// Newf 用错误码构造 error，自定义描述。
func Newf(c Code, format string, args ...any) *Error {
	return &Error{Code: c, Msg: fmt.Sprintf(format, args...)}
}

// Wrap 包装底层错误，保留错误码与可读描述，原始错误挂在 Cause 上。
func Wrap(c Code, cause error) *Error {
	return &Error{Code: c, Cause: cause}
}

// Wrapf 包装底层错误并自定义描述。
func Wrapf(c Code, cause error, format string, args ...any) *Error {
	return &Error{Code: c, Msg: fmt.Sprintf(format, args...), Cause: cause}
}

// CodeOf 从任意 error 中提取业务错误码；非业务错误返回 ErrInternal。
func CodeOf(err error) Code {
	if err == nil {
		return OK
	}
	var e *Error
	if as(err, &e) {
		return e.Code
	}
	return ErrInternal
}

// MessageOf 提取错误描述；非业务错误返回底层 error 的字符串。
func MessageOf(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if as(err, &e) {
		if e.Msg != "" {
			return e.Msg
		}
		return e.Code.Message()
	}
	return err.Error()
}
