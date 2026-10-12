package handler

import (
	"context"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	agentv1 "github.com/aegis/idl/kitex_gen/agentv1"
	agentservice "github.com/aegis/idl/kitex_gen/agentv1/agentservice"

	"github.com/aegis/pkg/logger"

	"github.com/aegis/gateway/internal/sse"
)

// agentStreamClient 把 Deps 适配为 sse 包所需的窄接口。
type agentStreamClient struct{ d *Deps }

var _ sse.AgentClient = agentStreamClient{}

// ChatStream 调用 agent_service 的流式对话
func (a agentStreamClient) ChatStream(ctx context.Context, req *agentv1.ChatReq) (agentservice.AgentService_ChatStreamClient, error) {
	return a.d.Agent.ChatStream(ctx, req)
}

// ---------- Agent CRUD ----------

// createAgentReq 创建 Agent 请求体
type createAgentReq struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Avatar            string   `json:"avatar"`
	SystemPrompt      string   `json:"system_prompt"`
	Model             string   `json:"model"`
	Temperature       float32  `json:"temperature"`
	MaxTokens         int32    `json:"max_tokens"`
	Tools             []string `json:"tools"`
	KbIds             []int64  `json:"kb_ids"`
	RagEnabled        bool     `json:"rag_enabled"`
	RagTopK           int32    `json:"rag_top_k"`
	RagScoreThreshold float64  `json:"rag_score_threshold"`
}

// CreateAgent 创建 Agent
func (d *Deps) CreateAgent(c *app.RequestContext) {
	var req createAgentReq
	if err := c.BindAndValidate(&req); err != nil || req.Name == "" {
		Error(c, consts.StatusBadRequest, "INVALID_PARAM", "name 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	// ★ 不从请求体取 owner_uid/tenant —— 身份以 context 为准
	resp, err := d.Agent.CreateAgent(ctx, &agentv1.CreateAgentReq{
		Agent: &agentv1.Agent{
			Name:              req.Name,
			Description:       req.Description,
			Avatar:            req.Avatar,
			SystemPrompt:      req.SystemPrompt,
			Model:             req.Model,
			Temperature:       req.Temperature,
			MaxTokens:         req.MaxTokens,
			Tools:             req.Tools,
			KbIds:             req.KbIds,
			RagEnabled:        req.RagEnabled,
			RagTopK:           req.RagTopK,
			RagScoreThreshold: req.RagScoreThreshold,
		},
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.CreateAgent", err)
		return
	}
	OK(c, utils.H{"agent_id": resp.GetAgentId(), "message": resp.GetMessage()})
}

// GetAgent 查询 Agent 详情
func (d *Deps) GetAgent(c *app.RequestContext) {
	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Agent.GetAgent(ctx, &agentv1.GetAgentReq{
		AgentId:  paramInt64(c, "agent_id"),
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.GetAgent", err)
		return
	}
	OK(c, resp.GetAgent())
}

// listAgentsReq 更新 Agent 请求体（update_mask 控制改动字段）
type listAgentsReq struct {
	Agent      *agentv1.Agent `json:"agent"`
	UpdateMask []string       `json:"update_mask"`
}

// UpdateAgent 更新 Agent
func (d *Deps) UpdateAgent(c *app.RequestContext) {
	agentID := paramInt64(c, "agent_id")
	if agentID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "agent_id 不能为空")
		return
	}

	var req listAgentsReq
	if err := c.BindAndValidate(&req); err != nil || req.Agent == nil {
		Error(c, consts.StatusBadRequest, "INVALID_PARAM", "agent 不能为空")
		return
	}
	// ★ 强制以路径参数为准，不接受 body 里的 agent_id。
	//   否则 PUT /agents/1001 带 body{"agent_id":2002}
	//   实际改的是 2002 —— 一个完全绕开 URL 语义的越权写入通道。
	req.Agent.AgentId = agentID

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Agent.UpdateAgent(ctx, &agentv1.UpdateAgentReq{
		Agent:      req.Agent,
		UpdateMask: req.UpdateMask,
		Identity:   idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.UpdateAgent", err)
		return
	}
	OK(c, resp.GetAgent())
}

// DeleteAgent 删除 Agent
func (d *Deps) DeleteAgent(c *app.RequestContext) {
	agentID := paramInt64(c, "agent_id")
	if agentID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "agent_id 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	if _, err := d.Agent.DeleteAgent(ctx, &agentv1.DeleteAgentReq{
		AgentId:  agentID,
		Identity: idOf(c),
	}); err != nil {
		d.handleErr(ctx, c, "agent.DeleteAgent", err)
		return
	}
	OK(c, utils.H{"message": "删除成功"})
}

// ListAgents 分页查询 Agent
func (d *Deps) ListAgents(c *app.RequestContext) {
	ctx, cancel := d.withTimeout(c)
	defer cancel()

	owner, tenant := ownerFilter(c)
	resp, err := d.Agent.ListAgents(ctx, &agentv1.ListAgentsReq{
		Page:     page(c),
		Keyword:  queryStr(c, "keyword"),
		OwnerUid: owner,
		// ★ tenant 取自身身份，不读 query：租户隔离是安全边界
		Tenant: tenant,
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.ListAgents", err)
		return
	}
	OK(c, utils.H{"agents": resp.GetAgents(), "meta": resp.GetMeta()})
}

// ---------- Session ----------

// CreateSession 创建会话
// createSessionReq 创建会话请求
type createSessionReq struct {
	AgentID int64  `json:"agent_id"`
	Title   string `json:"title"`
}

// CreateSession 创建会话
//
// agent_id 与 title 都走 JSON body：路由是 /api/v1/sessions，
// 没有 :agent_id 路径参数；title 放 query 只是为了迁就
// 一个不存在的路径参数，两者都不是好的接口设计。
func (d *Deps) CreateSession(c *app.RequestContext) {
	var req createSessionReq
	if err := c.BindAndValidate(&req); err != nil || req.AgentID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "agent_id 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Agent.CreateSession(ctx, &agentv1.CreateSessionReq{
		AgentId:  req.AgentID,
		Title:    req.Title,
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.CreateSession", err)
		return
	}
	OK(c, utils.H{"session_id": resp.GetSessionId(), "session": resp.GetSession()})
}

// GetSession 查询会话
func (d *Deps) GetSession(c *app.RequestContext) {
	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Agent.GetSession(ctx, &agentv1.GetSessionReq{
		SessionId: paramInt64(c, "session_id"),
		Identity:  idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.GetSession", err)
		return
	}
	OK(c, resp.GetSession())
}

// ListSessions 分页查询会话
func (d *Deps) ListSessions(c *app.RequestContext) {
	ctx, cancel := d.withTimeout(c)
	defer cancel()

	owner, tenant := ownerFilter(c)
	resp, err := d.Agent.ListSessions(ctx, &agentv1.ListSessionsReq{
		Page:    page(c),
		Uid:     owner,
		AgentId: queryInt64(c, "agent_id"),
		Tenant:  tenant,
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.ListSessions", err)
		return
	}
	OK(c, utils.H{"sessions": resp.GetSessions(), "meta": resp.GetMeta()})
}

// DeleteSession 删除会话
func (d *Deps) DeleteSession(c *app.RequestContext) {
	sessionID := paramInt64(c, "session_id")
	if sessionID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "session_id 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	if _, err := d.Agent.DeleteSession(ctx, &agentv1.DeleteSessionReq{
		SessionId: sessionID,
		Identity:  idOf(c),
	}); err != nil {
		d.handleErr(ctx, c, "agent.DeleteSession", err)
		return
	}
	OK(c, utils.H{"message": "删除成功"})
}

// ListMessages 分页查询消息
func (d *Deps) ListMessages(c *app.RequestContext) {
	sessionID := paramInt64(c, "session_id")
	if sessionID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "session_id 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Agent.ListMessages(ctx, &agentv1.ListMessagesReq{
		SessionId: sessionID,
		Page:      page(c),
		// 1=正序（时间线），2=倒序（只看最近）
		Direction: int32(queryInt(c, "direction", 2)),
		Identity:  idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.ListMessages", err)
		return
	}
	OK(c, utils.H{"messages": resp.GetMessages(), "meta": resp.GetMeta()})
}

// ---------- Chat ----------

// chatReq 对话请求体
type chatReq struct {
	SessionID int64   `json:"session_id"`
	AgentID   int64   `json:"agent_id"`
	Content   string  `json:"content"`
	Model     string  `json:"model"`
	UseRag    bool    `json:"use_rag"`
	KbIDs     []int64 `json:"kb_ids"`
	RequestID string  `json:"request_id"`
}

// Chat 同步对话
func (d *Deps) Chat(c *app.RequestContext) {
	req, ok := d.bindChat(c)
	if !ok {
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Agent.Chat(ctx, req)
	if err != nil {
		d.handleErr(ctx, c, "agent.Chat", err)
		return
	}
	OK(c, utils.H{
		"msg_id":      resp.GetMsgId(),
		"session_id":  resp.GetSessionId(),
		"message":     resp.GetMessage(),
		"degradation": resp.GetDegradation(),
		"retrieve_ms": resp.GetRetrieveMs(),
		"llm_ms":      resp.GetLlmMs(),
		"total_ms":    resp.GetTotalMs(),
	})
}

// ChatStream 流式对话（SSE）。
//
// ★ 令牌来源特殊说明：
//
//	浏览器 EventSource API 无法自定义请求头，
//	因此 access_token 必须走 query 参数。
//	这意味着令牌可能出现在网关访问日志里——
//	所以生产环境必须关闭 access_token 的日志记录（见 main.go）。
func (d *Deps) ChatStream(c *app.RequestContext) {
	req, ok := d.bindChat(c)
	if !ok {
		return
	}
	req.Stream = true

	sse.Forward(c, req, sse.Options{
		Client:    agentStreamClient{d: d},
		Timeout:   d.streamTimeout(),
		KeepAlive: keepAliveInterval,
		Retry:     3 * time.Second,
	})
}

// keepAliveInterval SSE 心跳间隔。
//
// 15s 是经验值：多数反向代理的空闲超时不短于 30s，
// 15s 心跳既能保活，又不会给网络带来可观负担。
const keepAliveInterval = 15 * time.Second

// bindChat 解析并校验对话请求
func (d *Deps) bindChat(c *app.RequestContext) (*agentv1.ChatReq, bool) {
	var req chatReq
	if err := c.BindAndValidate(&req); err != nil {
		Error(c, consts.StatusBadRequest, "INVALID_PARAM", "请求格式不正确")
		return nil, false
	}

	// ★ agent_id 一律以路径参数为准。
	//   允许 body 覆盖等于开了一条绕开 URL 的越权通道：
	//   POST /agents/1001/chat 带 body{"agent_id":2002}
	//   实际对话的是别人的 Agent。
	if aid := paramInt64(c, "agent_id"); aid != 0 {
		req.AgentID = aid
	}

	// EventSource 只能走query：Content 用 query 兜底
	if req.Content == "" {
		req.Content = queryStr(c, "content")
	}
	if sid := paramInt64(c, "session_id"); sid != 0 {
		req.SessionID = sid
	}

	// ★ 空白字符等同于空内容。
	//   只判 req.Content == "" 的话，"   " 会一路透传到 llm_service，
	//   被下游 TrimSpace 后拒绝，最终错误码映射成 503「模型服务不可用」——
	//   一个纯参数问题被报成服务故障，客户端会误以为该重试。
	//   这里与下游保持同一标准，让 400 在网关层就返回。
	if strings.TrimSpace(req.Content) == "" {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "content 不能为空")
		return nil, false
	}
	if req.AgentID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "agent_id 不能为空")
		return nil, false
	}

	return &agentv1.ChatReq{
		SessionId: req.SessionID,
		AgentId:   req.AgentID,
		Content:   req.Content,
		Model:     req.Model,
		UseRag:    req.UseRag,
		KbIds:     req.KbIDs,
		RequestId: req.RequestID,
		Identity:  idOf(c),
	}, true
}

// handleErr 按错误类型映射 HTTP 状态码。
//
// 下游 errno 已带语义，这里只需按 code 选状态码 ——
// 客户端才能区分"要重新登录"与"参数写错了"。
func (d *Deps) handleErr(ctx context.Context, c *app.RequestContext, op string, err error) {
	code := errnoCodeOf(err)
	status, msg := httpStatusOf(code)
	if status >= 500 {
		logger.Error(ctx, "下游调用失败", "op", op, "code", code, "err", err)
	} else {
		logger.Warn(ctx, "请求被拒绝", "op", op, "code", code)
	}
	Error(c, status, code, msg)
}

// ---------- Tool CRUD ----------
//
// 工具是管理员维护的全局资源，ReAct 循环按名字查 tools 表拿 endpoint/method 执行 HTTP 调用。

// createToolReq / updateToolReq 共用请求体
type toolReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Endpoint    string `json:"endpoint"`
	Method      string `json:"method"`
	ParamsSchema string `json:"params_schema"`
	TimeoutMs   int32  `json:"timeout_ms"`
	Status      int32  `json:"status"`
}

// CreateTool 新建工具（需 admin）
func (d *Deps) CreateTool(c *app.RequestContext) {
	var req toolReq
	if err := c.BindAndValidate(&req); err != nil || req.Name == "" {
		Error(c, consts.StatusBadRequest, "INVALID_PARAM", "name 不能为空")
		return
	}
	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Agent.CreateTool(ctx, &agentv1.CreateToolReq{
		Tool: &agentv1.Tool{
			Name:         req.Name,
			Description:  req.Description,
			Type:         req.Type,
			Endpoint:     req.Endpoint,
			Method:       req.Method,
			ParamsSchema: req.ParamsSchema,
			TimeoutMs:    req.TimeoutMs,
			Status:       req.Status,
		},
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.CreateTool", err)
		return
	}
	OK(c, utils.H{"tool_id": resp.GetToolId(), "message": resp.GetMessage()})
}

// GetTool 查询工具详情
func (d *Deps) GetTool(c *app.RequestContext) {
	ctx, cancel := d.withTimeout(c)
	defer cancel()
	resp, err := d.Agent.GetTool(ctx, &agentv1.GetToolReq{
		ToolId:   paramInt64(c, "tool_id"),
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.GetTool", err)
		return
	}
	OK(c, resp.GetTool())
}

// updateToolReq 带 update_mask 控制改动字段
type updateToolReq struct {
	Tool       *agentv1.Tool `json:"tool"`
	UpdateMask []string      `json:"update_mask"`
}

// UpdateTool 更新工具（需 admin）
func (d *Deps) UpdateTool(c *app.RequestContext) {
	toolID := paramInt64(c, "tool_id")
	if toolID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "tool_id 不能为空")
		return
	}
	var req updateToolReq
	if err := c.BindAndValidate(&req); err != nil || req.Tool == nil {
		Error(c, consts.StatusBadRequest, "INVALID_PARAM", "tool 不能为空")
		return
	}
	req.Tool.ToolId = toolID

	ctx, cancel := d.withTimeout(c)
	defer cancel()
	resp, err := d.Agent.UpdateTool(ctx, &agentv1.UpdateToolReq{
		Tool:       req.Tool,
		UpdateMask: req.UpdateMask,
		Identity:   idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.UpdateTool", err)
		return
	}
	OK(c, resp.GetTool())
}

// DeleteTool 删除工具（需 admin）
func (d *Deps) DeleteTool(c *app.RequestContext) {
	toolID := paramInt64(c, "tool_id")
	if toolID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "tool_id 不能为空")
		return
	}
	ctx, cancel := d.withTimeout(c)
	defer cancel()
	if _, err := d.Agent.DeleteTool(ctx, &agentv1.DeleteToolReq{
		ToolId:   toolID,
		Identity: idOf(c),
	}); err != nil {
		d.handleErr(ctx, c, "agent.DeleteTool", err)
		return
	}
	OK(c, utils.H{"message": "删除成功"})
}

// ListTools 分页查询工具
func (d *Deps) ListTools(c *app.RequestContext) {
	ctx, cancel := d.withTimeout(c)
	defer cancel()
	resp, err := d.Agent.ListTools(ctx, &agentv1.ListToolsReq{
		Page:    page(c),
		Keyword: queryStr(c, "keyword"),
		Type:    queryStr(c, "type"),
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.ListTools", err)
		return
	}
	OK(c, utils.H{"tools": resp.GetTools(), "meta": resp.GetMeta()})
}

// ---------- Agent↔Tool 绑定 ----------
//
// API 设计：
//   GET    /agents/:agent_id/tools           列出已绑定
//   PUT    /agents/:agent_id/tools           全量替换绑定
//   DELETE /agents/:agent_id/tools           解绑（body 可带 tool_names）

// bindAgentToolsReq 全量替换 Agent 的工具绑定
type bindAgentToolsReq struct {
	Bindings []*agentv1.AgentToolBinding `json:"bindings"`
}

// BindAgentTools 全量替换 Agent 的工具绑定（需 owner/admin）
func (d *Deps) BindAgentTools(c *app.RequestContext) {
	agentID := paramInt64(c, "agent_id")
	if agentID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "agent_id 不能为空")
		return
	}

	var req bindAgentToolsReq
	// 空 body 也算合法（清空全部绑定）
	_ = c.BindAndValidate(&req)

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	if _, err := d.Agent.BindAgentTools(ctx, &agentv1.BindAgentToolsReq{
		AgentId:  agentID,
		Bindings: req.Bindings,
		Identity: idOf(c),
	}); err != nil {
		d.handleErr(ctx, c, "agent.BindAgentTools", err)
		return
	}
	OK(c, utils.H{"message": "绑定成功", "count": len(req.Bindings)})
}

// unbindAgentToolsReq 解绑指定工具（空列表 = 解绑全部）
type unbindAgentToolsReq struct {
	ToolNames []string `json:"tool_names"`
}

// UnbindAgentTools 解绑 Agent 的工具（需 owner/admin）
func (d *Deps) UnbindAgentTools(c *app.RequestContext) {
	agentID := paramInt64(c, "agent_id")
	if agentID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "agent_id 不能为空")
		return
	}

	var req unbindAgentToolsReq
	_ = c.BindAndValidate(&req)

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	if _, err := d.Agent.UnbindAgentTools(ctx, &agentv1.UnbindAgentToolsReq{
		AgentId:   agentID,
		ToolNames: req.ToolNames,
		Identity:  idOf(c),
	}); err != nil {
		d.handleErr(ctx, c, "agent.UnbindAgentTools", err)
		return
	}
	OK(c, utils.H{"message": "解绑成功"})
}

// ListAgentTools 列出 Agent 已绑定的全部工具
func (d *Deps) ListAgentTools(c *app.RequestContext) {
	agentID := paramInt64(c, "agent_id")
	if agentID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "agent_id 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Agent.ListAgentTools(ctx, &agentv1.ListAgentToolsReq{
		AgentId:  agentID,
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "agent.ListAgentTools", err)
		return
	}
	OK(c, utils.H{"bindings": resp.GetBindings()})
}
