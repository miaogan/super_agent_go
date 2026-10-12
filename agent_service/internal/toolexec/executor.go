// Package toolexec —— Agent 工具执行器。
//
// 职责：把 tool_call（{name, arguments JSON}）转成一次 HTTP 调用，
// 返回 tool result JSON 供 Agent 再喂给 LLM。
//
// 设计：
//  1. ToolExecutor 是 Orchestrator 的依赖接口，测试用 fake 实现
//  2. httpExec 是生产实现：查工具定义 → fetch.DoStatus 执行
//     （SSRF 三层防护都在 fetch 里：前置校验 / dial 时 resolve-and-pin / 每跳重定向复核）
//  3. 不做 JSON Schema 校验（ParamsSchema 存着未用），LLM 出什么就调什么
//  4. 超时取工具 timeout_ms 与全局上限的较小值
//
// ★ 出站安全边界的来历（改坏过一次，注释同步错过一次）：
//
//	这里原本直接 http.DefaultClient.Do，并在注释里声称"仍保留 http.DefaultClient
//	里的安全 Transport（DialContext 二次校验）"。那是不存在的：DefaultClient 用的是
//	标准库裸 Transport，没有任何 IP 校验；而且它默认跟随最多 10 次重定向且
//	完全不校验 Location —— 一个 302 就能把请求送到 169.254.169.254。
//
//	当时的前提"endpoint 来自 PG tools 表（可信）"也不成立：admin 角色可以
//	通过 POST /api/v1/tools 写这张表（见 gateway/internal/router/router.go:198），
//	所以可写配置 = 攻击面。现在按不可信输入处理，内网目标默认拒绝。
package toolexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/fetch"
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
	Type      string
	Endpoint  string
	Method    string
	TimeoutMS int
	// ParamsSchema 工具入参 JSON Schema（这轮不校验，存着）
	ParamsSchema string
}

// ToolRegistry 工具定义查询。
// 从 PG tools 表读一条；agent_service 的 pgstore 可以实现这个接口。
type ToolRegistry interface {
	GetTool(ctx context.Context, name string) (*ToolDef, error)
}

// Config 执行器策略。
type Config struct {
	// Timeout 单次调用的全局上限（工具自身 timeout_ms 再小也受它约束）。
	// <=0 时取 30s。
	Timeout time.Duration
	// MaxResponseBytes 响应体上限，<=0 时取 1MB。
	MaxResponseBytes int64
	// AllowPrivateIPs 是否允许工具指向内网/保留地址。
	//
	// 生产必须 false。为 true 等于把内网（含云元数据地址）暴露给
	// 任何能写 tools 表的角色，只在完全隔离的本地环境才考虑。
	AllowPrivateIPs bool
	// AllowHosts 例外主机名单：逐个放行确可信的联调目标
	// （如 host.docker.internal）。需要内网时优先用这个，
	// 而不是把 AllowPrivateIPs 打开。
	AllowHosts []string
}

const (
	defaultTimeout    = 30 * time.Second
	defaultMaxResp    = 1 << 20 // 1MB
	defaultToolTimout = 5 * time.Second
)

// ErrToolNotFound 工具名不存在。
var ErrToolNotFound = errno.New(errno.ErrNotFound)

// ErrToolExec 工具执行失败。
var ErrToolExec = errno.New(errno.ErrInternal)

// ErrToolBlocked 工具 endpoint 被 SSRF 策略拦截。
//
// 单独一个错误类型：这是「配置不合法」而不是「上游挂了」，
// 混在 ErrToolExec 里会让排障时把安全拦截误判成工具服务故障。
var ErrToolBlocked = errno.New(errno.ErrForbidden)

// httpExec 默认 HTTP 工具执行器。
type httpExec struct {
	reg    ToolRegistry
	opt    fetch.Options
	client *http.Client
}

// NewHTTP 构造生产 HTTP 工具执行器。
//
// client 在此一次性建好并复用：ReAct 循环里一次回答可能连打多个工具，
// 每次新建 Transport 会让 keep-alive 与空闲连接全部作废。
func NewHTTP(reg ToolRegistry, cfg Config) ToolExecutor {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = defaultMaxResp
	}
	policy := ssrf.Policy{
		AllowPrivateIPs: cfg.AllowPrivateIPs,
		AllowedSchemes:  []string{"http", "https"},
		DNSRecheckCount: 3,
		AllowHosts:      cfg.AllowHosts,
	}
	opt := fetch.Options{
		Timeout:      cfg.Timeout,
		MaxSizeBytes: cfg.MaxResponseBytes,
		SSRF:         policy,
		UserAgent:    "Aegis-ToolExecutor/1.0",
	}
	return &httpExec{reg: reg, opt: opt, client: fetch.NewClient(opt)}
}

func (h *httpExec) Execute(ctx context.Context, toolName string, argumentsJSON string) (string, error) {
	def, err := h.reg.GetTool(ctx, toolName)
	if err != nil {
		return "", err
	}
	if def.Type != "" && def.Type != "http" {
		return "", fmt.Errorf("tool %q type=%s 暂不支持", toolName, def.Type)
	}

	// 工具声明的超时与全局上限取较小值：防止一条 timeout_ms=99999999
	// 的记录把 ReAct 循环整条挂死。
	timeout := time.Duration(def.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultToolTimout
	}
	if timeout > h.opt.Timeout {
		timeout = h.opt.Timeout
	}
	opt := h.opt
	opt.Timeout = timeout

	method := strings.ToUpper(def.Method)
	if method == "" {
		method = http.MethodPost
	}

	var body []byte
	if argumentsJSON != "" && method != http.MethodGet && method != http.MethodHead {
		body = []byte(argumentsJSON)
	}

	resp, err := fetch.DoStatus(ctx, h.client, method, def.Endpoint, body, opt)
	if err != nil {
		// 被 SSRF 拦掉时不要退化成"工具执行失败"：endpoint 是管理员写进
		// tools 表的，被拦说明那条记录本身不合法，得能从日志里分辨出来。
		if errors.Is(err, fetch.ErrBlocked) {
			logger.Warn(ctx, "工具 endpoint 被 SSRF 策略拦截",
				"tool", toolName, "err", err)
			return "", fmt.Errorf("tool %q endpoint 不合法: %w", toolName, ErrToolBlocked)
		}
		logger.Warn(ctx, "工具执行失败", "tool", toolName, "err", err)
		return "", fmt.Errorf("tool %q 执行失败: %w", toolName, ErrToolExec)
	}

	if resp.StatusCode >= 400 {
		logger.Warn(ctx, "工具返回错误状态码", "tool", toolName, "status", resp.StatusCode)
		return "", fmt.Errorf("tool %q 返回 %d: %s", toolName, resp.StatusCode, truncate(string(resp.Body), 500))
	}

	// 返回 JSON（解析校验一下确保是合法 JSON 或纯文本）
	result := string(resp.Body)
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
