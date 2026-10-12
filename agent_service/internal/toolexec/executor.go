// Package toolexec — Agent 工具执行器。
//
// 职责：把 tool_call（{name, arguments JSON}）转成一次 HTTP 调用，
// 返回 tool result JSON 供 Agent 再喂给 LLM。
//
// 设计：
//   1. ToolExecutor 是 Orchestrator 的依赖接口，测试用 fake 实现
//   2. httpExec 是生产实现：查工具定义 → pkg/fetch 执行（SSRF 防护在 fetch 里）
//   3. 不做 JSON Schema 校验（生产可加），LLM 出什么就调什么
//   4. 超时从工具定义 timeout_ms 读 + 全局 30s 上限
package toolexec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/ssrf"
)

// ToolExecutor Agent 工具执行能力。
//
// Execute 把一个 tool_call 转成 HTTP 调用，返回 JSON 结果字符串。
// 生产实现会查工具注册表拿到 endpoint / method / timeout。
//
// 返回的 result 会被 Orchestrator 作为 role="tool" 的 ChatMessage.content 落库，
// 然后喂回 LLM 的 messages 数组——所以内容必须是 LLM 能理解的 JSON。
type ToolExecutor interface {
	Execute(ctx context.Context, toolName string, argumentsJSON string) (resultJSON string, err error)
}

// ToolDef 工具运行时定义（从 PG tools 表读出来）。
type ToolDef struct {
	Name        string
	Description string
	// Type 当前只支持 "http"
	Type        string
	Endpoint    string
	Method      string
	TimeoutMS   int
	// ParamsSchema 工具入参 JSON Schema（这轮不校验，存着）
	ParamsSchema string
}

// ToolRegistry 工具定义查询。
// 从 PG tools 表读一条；agent_service 的 pgstore 可以实现这个接口。
type ToolRegistry interface {
	GetTool(ctx context.Context, name string) (*ToolDef, error)
}

// ErrToolNotFound 工具名不存在。
var ErrToolNotFound = errno.New(errno.ErrNotFound)

// ErrToolExec 工具执行失败。
var ErrToolExec = errno.New(errno.ErrInternal)

// httpExec 默认 HTTP 工具执行器。
type httpExec struct {
	reg          ToolRegistry
	fetchTimeout time.Duration // 全局上限，覆盖工具 timeout
}

// NewHTTP 构造生产 HTTP 工具执行器。
// fetchTimeout 为 0 时用默认 30s。
func NewHTTP(reg ToolRegistry, fetchTimeout time.Duration) ToolExecutor {
	if fetchTimeout <= 0 {
		fetchTimeout = 30 * time.Second
	}
	return &httpExec{reg: reg, fetchTimeout: fetchTimeout}
}

func (h *httpExec) Execute(ctx context.Context, toolName string, argumentsJSON string) (string, error) {
	def, err := h.reg.GetTool(ctx, toolName)
	if err != nil {
		return "", err
	}
	if def.Type != "" && def.Type != "http" {
		return "", fmt.Errorf("tool %q type=%s 暂不支持", toolName, def.Type)
	}

	timeoutMs := def.TimeoutMS
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout > h.fetchTimeout {
		timeout = h.fetchTimeout
	}

	// SSRF 校验：endpoint 来自 PG tools 表（可信），跳过重拦截。
	// 仍然保留 http.DefaultClient 里的安全 Transport（DialContext 二次校验）。
	policy := ssrf.Default
	policy.AllowPrivateIPs = true
	if _, err := ssrf.Validate(ctx, def.Endpoint, policy); err != nil {
		return "", fmt.Errorf("tool %q endpoint %w", toolName, err)
	}

	method := strings.ToUpper(def.Method)
	if method == "" {
		method = "POST"
	}

	var body io.Reader
	if argumentsJSON != "" && method != http.MethodGet && method != http.MethodHead {
		body = strings.NewReader(argumentsJSON)
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, def.Endpoint, body)
	if err != nil {
		return "", fmt.Errorf("tool %q 构造请求失败: %w", toolName, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Warn(ctx, "工具执行失败", "tool", toolName, "err", err)
		return "", fmt.Errorf("tool %q 执行失败: %w", toolName, ErrToolExec)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 上限 1MB
	if err != nil {
		return "", fmt.Errorf("tool %q 读取响应失败: %w", toolName, err)
	}

	if resp.StatusCode >= 400 {
		logger.Warn(ctx, "工具返回错误状态码", "tool", toolName, "status", resp.StatusCode)
		return "", fmt.Errorf("tool %q 返回 %d: %s", toolName, resp.StatusCode, truncate(string(respBody), 500))
	}

	// 返回 JSON（解析校验一下确保是合法 JSON 或纯文本）
	result := string(respBody)
	if !json.Valid([]byte(result)) {
		// 非 JSON 响应也可以，LLM 照样能理解
		result = fmt.Sprintf("%q", result)
	}
	return result, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
