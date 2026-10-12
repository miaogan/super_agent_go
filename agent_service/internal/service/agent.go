// Package service 实现 agent_service 的 Kitex handler。
//
// 职责边界：
//   - 参数校验与身份提取
//   - 调用 store / orchestrate
//   - pb ↔ 领域对象的转换
//
// 不放业务逻辑：编排决策都在 orchestrate，持久化都在 store。
// 这样两层都能独立测试，handler 只是薄薄一层适配。
package service

import (
	"context"

	agentv1 "github.com/aegis/idl/kitex_gen/agentv1"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"

	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/identity"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/mockx"
	"github.com/aegis/pkg/pagination"

	"github.com/aegis/agent_service/internal/orchestrate"
	"github.com/aegis/agent_service/internal/store"
)

// AgentService agent_service 的 Kitex 实现
type AgentService struct {
	store store.Repository
	orch  *orchestrate.Orchestrator
	mock  *mockx.FaultInjector
}

// New 构造 handler。st 为内存版或 PG 版仓储（main 装配时决定）
func New(st store.Repository, orch *orchestrate.Orchestrator, mock *mockx.FaultInjector) *AgentService {
	return &AgentService{store: st, orch: orch, mock: mock}
}

// ---------- 错误映射 ----------

// mapErr 把仓储错误映射为平台错误码。
//
// 区分 not_found / conflict / forbidden，
// 让网关能返回正确的 HTTP 状态码而不是一律 500。
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	switch err.(type) {
	case *store.ErrNotFound:
		return errno.Wrap(errno.ErrNotFound, err)
	case *store.ErrConflict:
		return errno.Wrap(errno.ErrConflict, err)
	case *store.ErrForbidden:
		return errno.Wrap(errno.ErrForbidden, err)
	default:
		return errno.Wrap(errno.ErrInternal, err)
	}
}

// withID 把 req.Identity 注入 ctx（如果有）。
// Kitex v0.16 的 server middleware 对 protobuf unary RPC 拿不到真实 req 类型，
// 这里在 handler 入口显式注入，让 identity.From(ctx) 后续调用始终有效。
func withID(ctx context.Context, req interface{}) context.Context {
	if p, ok := req.(identity.IdentityProvider); ok {
		return identity.With(ctx, p.GetIdentity())
	}
	return ctx
}

// ---------- Health ----------

// Health 健康检查
func (s *AgentService) Health(ctx context.Context, _ *commonv1.HealthReq) (*commonv1.HealthResp, error) {
	st := s.store.Stats(ctx)
	logger.Debug(ctx, "agent_service health",
		"agents", st["agents"], "sessions", st["sessions"], "messages", st["messages"])

	return &commonv1.HealthResp{
		Healthy:  true,
		Service:  "agent_service",
		MockMode: s.mock.Enabled(),
	}, nil
}

// ---------- Agent CRUD ----------

// CreateAgent 新建 Agent
func (s *AgentService) CreateAgent(ctx context.Context, req *agentv1.CreateAgentReq) (*agentv1.CreateAgentResp, error) {
	ctx = withID(ctx, req)
	in := req.GetAgent()
	if in == nil {
		return nil, errno.Newf(errno.ErrInvalidParam, "agent 不能为空")
	}
	if in.Name == "" {
		return nil, errno.Newf(errno.ErrInvalidParam, "name 不能为空")
	}

	id := identity.From(ctx)
	// ★ 身份一律以 context 为准，不接受请求体自报。
	//   否则任何人都能伪造 owner_uid 创建"他人 Agent"，
	//   或者把 tenant 改成别的租户实现越权写入。
	cp := &agentv1.Agent{
		Name:              in.Name,
		Description:       in.Description,
		Avatar:            in.Avatar,
		SystemPrompt:      in.SystemPrompt,
		Model:             in.Model,
		Temperature:       in.Temperature,
		MaxTokens:         in.MaxTokens,
		Tools:             in.Tools,
		KbIds:             in.KbIds,
		RagEnabled:        in.RagEnabled,
		RagTopK:           in.RagTopK,
		RagScoreThreshold: in.RagScoreThreshold,
		OwnerUid:          id.GetUid(),
		Tenant:            id.GetTenant(),
		Status:            in.Status,
	}

	agentID, err := s.store.CreateAgent(ctx, cp)
	if err != nil {
		return nil, mapErr(err)
	}
	logger.Info(ctx, "agent created", "agent_id", agentID, "name", in.Name)

	return &agentv1.CreateAgentResp{
		Success: true,
		AgentId: agentID,
		Message: "创建成功",
	}, nil
}

// GetAgent 读取 Agent
func (s *AgentService) GetAgent(ctx context.Context, req *agentv1.GetAgentReq) (*agentv1.GetAgentResp, error) {
	ctx = withID(ctx, req)
	agent, err := s.store.GetAgent(ctx, req.GetAgentId())
	if err != nil {
		return nil, mapErr(err)
	}
	if err := checkOwner(ctx, agent); err != nil {
		return nil, err
	}
	return &agentv1.GetAgentResp{Success: true, Agent: agent}, nil
}

// UpdateAgent 更新 Agent
func (s *AgentService) UpdateAgent(ctx context.Context, req *agentv1.UpdateAgentReq) (*agentv1.UpdateAgentResp, error) {
	ctx = withID(ctx, req)
	old, err := s.store.GetAgent(ctx, req.GetAgent().GetAgentId())
	if err != nil {
		return nil, mapErr(err)
	}
	if err := checkOwner(ctx, old); err != nil {
		return nil, err
	}
	// owner_uid / tenant 不允许通过更新接口改写 —— 那是创建时确定的归属
	req.Agent.OwnerUid = old.OwnerUid
	req.Agent.Tenant = old.Tenant

	agent, err := s.store.UpdateAgent(ctx, req.GetAgent(), req.GetUpdateMask())
	if err != nil {
		return nil, mapErr(err)
	}
	return &agentv1.UpdateAgentResp{Success: true, Agent: agent}, nil
}

// DeleteAgent 删除 Agent
func (s *AgentService) DeleteAgent(ctx context.Context, req *agentv1.DeleteAgentReq) (*commonv1.OpResult, error) {
	ctx = withID(ctx, req)
	agent, err := s.store.GetAgent(ctx, req.GetAgentId())
	if err != nil {
		return nil, mapErr(err)
	}
	if err := checkOwner(ctx, agent); err != nil {
		return nil, err
	}
	if err := s.store.DeleteAgent(ctx, req.GetAgentId()); err != nil {
		return nil, mapErr(err)
	}
	return &commonv1.OpResult{Success: true, Message: "删除成功"}, nil
}

// ListAgents 分页查询 Agent
func (s *AgentService) ListAgents(ctx context.Context, req *agentv1.ListAgentsReq) (*agentv1.ListAgentsResp, error) {
	ctx = withID(ctx, req)
	id := identity.From(ctx)

	ownerUID := req.GetOwnerUid()
	tenant := req.GetTenant()

	//★ 越权防护：普通用户只能看自己的。
	//   即使请求里显式传了别人的 owner_uid，
	//   非 admin 也必须被降级为"只看自己"。
	if !identity.IsAdmin(ctx) {
		ownerUID = id.GetUid()
		tenant = id.GetTenant()
	}
	if ownerUID == 0 && tenant == "" {
		tenant = id.GetTenant()
	}

	page, size := pagination.Normalize(req.GetPage())
	agents, total := s.store.ListAgents(ctx, req.GetKeyword(), ownerUID, tenant, page, size)

	return &agentv1.ListAgentsResp{
		Agents: agents,
		Meta:   pagination.Meta(req.GetPage(), total),
	}, nil
}

// ---------- Tool CRUD ----------
//
// 工具是管理员维护的全局资源——运行时 ReAct 循环按名字查 tools 表拿 endpoint/method 执行 HTTP 调用。
// 普通用户只读（ListTools/GetToolByID），写入（CreateTool/UpdateTool/DeleteTool）需 admin 角色。

// CreateTool 新建工具（需 admin）
func (s *AgentService) CreateTool(ctx context.Context, req *agentv1.CreateToolReq) (*agentv1.CreateToolResp, error) {
	ctx = withID(ctx, req)
	if !identity.IsAdmin(ctx) {
		return nil, errno.Newf(errno.ErrForbidden, "需要管理员权限")
	}
	in := req.GetTool()
	if in == nil {
		return nil, errno.Newf(errno.ErrInvalidParam, "tool 不能为空")
	}
	if in.Name == "" {
		return nil, errno.Newf(errno.ErrInvalidParam, "name 不能为空")
	}
	id, err := s.store.CreateTool(ctx, in)
	if err != nil {
		return nil, mapErr(err)
	}
	logger.Info(ctx, "tool created", "tool_id", id, "name", in.Name)
	return &agentv1.CreateToolResp{Success: true, ToolId: id, Message: "创建成功"}, nil
}

// GetTool 按 ID 读取工具
func (s *AgentService) GetTool(ctx context.Context, req *agentv1.GetToolReq) (*agentv1.GetToolResp, error) {
	ctx = withID(ctx, req)
	tool, err := s.store.GetToolByID(ctx, req.GetToolId())
	if err != nil {
		return nil, mapErr(err)
	}
	return &agentv1.GetToolResp{Success: true, Tool: tool}, nil
}

// UpdateTool 更新工具（需 admin）
func (s *AgentService) UpdateTool(ctx context.Context, req *agentv1.UpdateToolReq) (*agentv1.UpdateToolResp, error) {
	ctx = withID(ctx, req)
	if !identity.IsAdmin(ctx) {
		return nil, errno.Newf(errno.ErrForbidden, "需要管理员权限")
	}
	tool, err := s.store.UpdateTool(ctx, req.GetTool(), req.GetUpdateMask())
	if err != nil {
		return nil, mapErr(err)
	}
	return &agentv1.UpdateToolResp{Success: true, Tool: tool}, nil
}

// DeleteTool 删除工具（需 admin）
func (s *AgentService) DeleteTool(ctx context.Context, req *agentv1.DeleteToolReq) (*commonv1.OpResult, error) {
	ctx = withID(ctx, req)
	if !identity.IsAdmin(ctx) {
		return nil, errno.Newf(errno.ErrForbidden, "需要管理员权限")
	}
	if err := s.store.DeleteTool(ctx, req.GetToolId()); err != nil {
		return nil, mapErr(err)
	}
	return &commonv1.OpResult{Success: true, Message: "删除成功"}, nil
}

// ListTools 分页查询工具（所有登录用户可读）
func (s *AgentService) ListTools(ctx context.Context, req *agentv1.ListToolsReq) (*agentv1.ListToolsResp, error) {
	ctx = withID(ctx, req)
	page, size := pagination.Normalize(req.GetPage())
	tools, total := s.store.ListTools(ctx, req.GetKeyword(), req.GetType(), page, size)
	return &agentv1.ListToolsResp{
		Tools: tools,
		Meta:  pagination.Meta(req.GetPage(), total),
	}, nil
}

// ---------- Agent↔Tool 绑定 ----------
//
// 写入操作（Bind/Unbind）需要 owner 或 admin；List 只要登录即可。

// BindAgentTools 全量替换 Agent 的工具绑定（需 owner/admin）。
func (s *AgentService) BindAgentTools(ctx context.Context, req *agentv1.BindAgentToolsReq) (*commonv1.OpResult, error) {
	ctx = withID(ctx, req)
	agent, err := s.store.GetAgent(ctx, req.GetAgentId())
	if err != nil {
		return nil, mapErr(err)
	}
	if err := checkOwner(ctx, agent); err != nil {
		return nil, err
	}

	if err := s.store.BindAgentTools(ctx, req.GetAgentId(), req.GetBindings()); err != nil {
		return nil, mapErr(err)
	}
	logger.Info(ctx, "agent tools bound", "agent_id", req.GetAgentId(), "count", len(req.GetBindings()))
	return &commonv1.OpResult{Success: true, Message: "绑定成功"}, nil
}

// UnbindAgentTools 解绑指定工具（需 owner/admin）
func (s *AgentService) UnbindAgentTools(ctx context.Context, req *agentv1.UnbindAgentToolsReq) (*commonv1.OpResult, error) {
	ctx = withID(ctx, req)
	agent, err := s.store.GetAgent(ctx, req.GetAgentId())
	if err != nil {
		return nil, mapErr(err)
	}
	if err := checkOwner(ctx, agent); err != nil {
		return nil, err
	}

	if err := s.store.UnbindAgentTools(ctx, req.GetAgentId(), req.GetToolNames()); err != nil {
		return nil, mapErr(err)
	}
	return &commonv1.OpResult{Success: true, Message: "解绑成功"}, nil
}

// ListAgentTools 列出 Agent 已绑定的全部工具
func (s *AgentService) ListAgentTools(ctx context.Context, req *agentv1.ListAgentToolsReq) (*agentv1.ListAgentToolsResp, error) {
	ctx = withID(ctx, req)
	agent, err := s.store.GetAgent(ctx, req.GetAgentId())
	if err != nil {
		return nil, mapErr(err)
	}
	if err := checkOwner(ctx, agent); err != nil {
		return nil, err
	}

	bindings, err := s.store.ListAgentTools(ctx, req.GetAgentId())
	if err != nil {
		return nil, mapErr(err)
	}
	return &agentv1.ListAgentToolsResp{Bindings: bindings}, nil
}

// ---------- Session ----------

// CreateSession 新建会话
func (s *AgentService) CreateSession(ctx context.Context, req *agentv1.CreateSessionReq) (*agentv1.CreateSessionResp, error) {
	ctx = withID(ctx, req)
	if req.GetAgentId() == 0 {
		return nil, errno.Newf(errno.ErrInvalidParam, "agent_id 不能为空")
	}
	agent, err := s.store.GetAgent(ctx, req.GetAgentId())
	if err != nil {
		return nil, mapErr(err)
	}
	if err := checkOwner(ctx, agent); err != nil {
		return nil, err
	}

	sess, err := s.store.CreateSession(ctx, req.GetAgentId(), req.GetTitle(), identity.From(ctx))
	if err != nil {
		return nil, mapErr(err)
	}
	return &agentv1.CreateSessionResp{
		Success: true, SessionId: sess.SessionId, Session: sess,
	}, nil
}

// GetSession 读取会话
func (s *AgentService) GetSession(ctx context.Context, req *agentv1.GetSessionReq) (*agentv1.GetSessionResp, error) {
	ctx = withID(ctx, req)
	sess, err := s.store.GetSession(ctx, req.GetSessionId())
	if err != nil {
		return nil, mapErr(err)
	}
	if err := checkSessionOwner(ctx, sess); err != nil {
		return nil, err
	}
	return &agentv1.GetSessionResp{Success: true, Session: sess}, nil
}

// ListSessions 分页查询会话
func (s *AgentService) ListSessions(ctx context.Context, req *agentv1.ListSessionsReq) (*agentv1.ListSessionsResp, error) {
	ctx = withID(ctx, req)
	id := identity.From(ctx)
	uid := req.GetUid()
	tenant := req.GetTenant()

	// 同 ListAgents：普通用户强制只看自己
	if !identity.IsAdmin(ctx) {
		uid = id.GetUid()
		tenant = id.GetTenant()
	}
	// 匿名调用不应看到任何会话
	if uid == 0 && tenant == "" {
		tenant = id.GetTenant()
	}

	page, size := pagination.Normalize(req.GetPage())
	sessions, total := s.store.ListSessions(ctx, uid, req.GetAgentId(), tenant, page, size)

	return &agentv1.ListSessionsResp{
		Sessions: sessions,
		Meta:     pagination.Meta(req.GetPage(), total),
	}, nil
}

// DeleteSession 删除会话
func (s *AgentService) DeleteSession(ctx context.Context, req *agentv1.DeleteSessionReq) (*commonv1.OpResult, error) {
	ctx = withID(ctx, req)
	sess, err := s.store.GetSession(ctx, req.GetSessionId())
	if err != nil {
		return nil, mapErr(err)
	}
	if err := checkSessionOwner(ctx, sess); err != nil {
		return nil, err
	}
	if err := s.store.DeleteSession(ctx, req.GetSessionId()); err != nil {
		return nil, mapErr(err)
	}
	return &commonv1.OpResult{Success: true, Message: "删除成功"}, nil
}

// ListMessages 分页查询消息
func (s *AgentService) ListMessages(ctx context.Context, req *agentv1.ListMessagesReq) (*agentv1.ListMessagesResp, error) {
	ctx = withID(ctx, req)
	sess, err := s.store.GetSession(ctx, req.GetSessionId())
	if err != nil {
		return nil, mapErr(err)
	}
	if err := checkSessionOwner(ctx, sess); err != nil {
		return nil, err
	}

	page, size := pagination.Normalize(req.GetPage())
	msgs, total := s.store.ListMessages(ctx, req.GetSessionId(), req.GetDirection(), page, size)

	return &agentv1.ListMessagesResp{
		Messages: msgs,
		Meta:     pagination.Meta(req.GetPage(), total),
	}, nil
}

// ---------- Chat ----------

// Chat 同步对话
func (s *AgentService) Chat(ctx context.Context, req *agentv1.ChatReq) (*agentv1.ChatResp, error) {
	ctx = withID(ctx, req)
	if req.GetContent() == "" {
		return nil, errno.Newf(errno.ErrInvalidParam, "content 不能为空")
	}

	agent, sessionID, err := s.resolve(ctx, req)
	if err != nil {
		return nil, err
	}

	// 用户消息先落库，保证上下文完整
	if _, err := s.store.AppendMessage(ctx, sessionID, &agentv1.ChatMessage{
		Role:    "user",
		Content: req.GetContent(),
	}); err != nil {
		logger.Error(ctx, "保存用户消息失败", "err", err)
	}

	res, err := s.orch.Chat(ctx, agent, req, sessionID, identity.From(ctx))
	if err != nil {
		return nil, errno.Wrap(errno.ErrModelUnavailable, err)
	}

	return &agentv1.ChatResp{
		Success:     true,
		MsgId:       res.Message.GetMsgId(),
		SessionId:   sessionID,
		Message:     res.Message,
		Degradation: res.Degraded,
		RetrieveMs:  res.RetrieveMS,
		LlmMs:       res.LLMMS,
		TotalMs:     res.TotalMS,
	}, nil
}

// resolve 解析并校验 Chat 请求的目标 Agent 与 Session。
//
// session_id = 0 时自动新建会话 ——
// 免去前端为了发第一条消息先调一次 CreateSession 的往返。
func (s *AgentService) resolve(ctx context.Context, req *agentv1.ChatReq) (*agentv1.Agent, int64, error) {
	agentID := req.GetAgentId()
	if agentID == 0 {
		return nil, 0, errno.Newf(errno.ErrInvalidParam, "agent_id 不能为空")
	}
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return nil, 0, mapErr(err)
	}
	if err := checkOwner(ctx, agent); err != nil {
		return nil, 0, err
	}
	if agent.GetStatus() != store.AgentStatusEnabled {
		return nil, 0, errno.Newf(errno.ErrForbidden, "该 Agent 已停用")
	}

	sessionID := req.GetSessionId()
	if sessionID == 0 {
		sess, err := s.store.CreateSession(ctx, agentID, "", identity.From(ctx))
		if err != nil {
			return nil, 0, mapErr(err)
		}
		return agent, sess.SessionId, nil
	}

	// 必须校验会话归属 —— 否则改个 session_id 就能读别人的对话
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, 0, mapErr(err)
	}
	if err := checkSessionOwner(ctx, sess); err != nil {
		return nil, 0, err
	}
	if sess.GetAgentId() != agentID {
		return nil, 0, errno.Newf(errno.ErrInvalidParam, "会话不属于该 Agent")
	}
	return agent, sessionID, nil
}

// ---------- 鉴权辅助 ----------

// checkOwner 校验操作者是否为 Agent 所有者或管理员。
//
// ★ OwnerUid == 0 的 Agent 视为系统内置，任何登录用户可用；
//
//	其余必须本人或admin，否则返回 403。
func checkOwner(ctx context.Context, agent *agentv1.Agent) error {
	id := identity.From(ctx)
	if agent.GetOwnerUid() == 0 {
		return nil // 公共 Agent
	}
	if agent.GetOwnerUid() == id.GetUid() {
		return nil
	}
	if identity.IsAdmin(ctx) {
		return nil
	}
	return errno.Newf(errno.ErrForbidden, "无权操作该 Agent")
}

// checkSessionOwner 校验会话归属
func checkSessionOwner(ctx context.Context, sess *agentv1.Session) error {
	id := identity.From(ctx)
	if sess.GetUid() == id.GetUid() {
		return nil
	}
	if identity.IsAdmin(ctx) {
		return nil
	}
	return errno.Newf(errno.ErrForbidden, "无权访问该会话")
}
