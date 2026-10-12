package main

import (
	"context"
	agentv1 "github.com/aegis/idl/kitex_gen/agentv1"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
)

// AgentServiceImpl implements the last service interface defined in the IDL.
type AgentServiceImpl struct{}

// Health implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) Health(ctx context.Context, req *commonv1.HealthReq) (resp *commonv1.HealthResp, err error) {
	// TODO: Your code here...
	return
}

// CreateAgent implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) CreateAgent(ctx context.Context, req *agentv1.CreateAgentReq) (resp *agentv1.CreateAgentResp, err error) {
	// TODO: Your code here...
	return
}

// GetAgent implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) GetAgent(ctx context.Context, req *agentv1.GetAgentReq) (resp *agentv1.GetAgentResp, err error) {
	// TODO: Your code here...
	return
}

// UpdateAgent implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) UpdateAgent(ctx context.Context, req *agentv1.UpdateAgentReq) (resp *agentv1.UpdateAgentResp, err error) {
	// TODO: Your code here...
	return
}

// DeleteAgent implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) DeleteAgent(ctx context.Context, req *agentv1.DeleteAgentReq) (resp *commonv1.OpResult, err error) {
	// TODO: Your code here...
	return
}

// ListAgents implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) ListAgents(ctx context.Context, req *agentv1.ListAgentsReq) (resp *agentv1.ListAgentsResp, err error) {
	// TODO: Your code here...
	return
}

// CreateTool implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) CreateTool(ctx context.Context, req *agentv1.CreateToolReq) (resp *agentv1.CreateToolResp, err error) {
	// TODO: Your code here...
	return
}

// GetTool implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) GetTool(ctx context.Context, req *agentv1.GetToolReq) (resp *agentv1.GetToolResp, err error) {
	// TODO: Your code here...
	return
}

// UpdateTool implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) UpdateTool(ctx context.Context, req *agentv1.UpdateToolReq) (resp *agentv1.UpdateToolResp, err error) {
	// TODO: Your code here...
	return
}

// DeleteTool implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) DeleteTool(ctx context.Context, req *agentv1.DeleteToolReq) (resp *commonv1.OpResult, err error) {
	// TODO: Your code here...
	return
}

// ListTools implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) ListTools(ctx context.Context, req *agentv1.ListToolsReq) (resp *agentv1.ListToolsResp, err error) {
	// TODO: Your code here...
	return
}

// BindAgentTools implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) BindAgentTools(ctx context.Context, req *agentv1.BindAgentToolsReq) (resp *commonv1.OpResult, err error) {
	// TODO: Your code here...
	return
}

// UnbindAgentTools implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) UnbindAgentTools(ctx context.Context, req *agentv1.UnbindAgentToolsReq) (resp *commonv1.OpResult, err error) {
	// TODO: Your code here...
	return
}

// ListAgentTools implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) ListAgentTools(ctx context.Context, req *agentv1.ListAgentToolsReq) (resp *agentv1.ListAgentToolsResp, err error) {
	// TODO: Your code here...
	return
}

// CreateSession implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) CreateSession(ctx context.Context, req *agentv1.CreateSessionReq) (resp *agentv1.CreateSessionResp, err error) {
	// TODO: Your code here...
	return
}

// GetSession implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) GetSession(ctx context.Context, req *agentv1.GetSessionReq) (resp *agentv1.GetSessionResp, err error) {
	// TODO: Your code here...
	return
}

// ListSessions implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) ListSessions(ctx context.Context, req *agentv1.ListSessionsReq) (resp *agentv1.ListSessionsResp, err error) {
	// TODO: Your code here...
	return
}

// DeleteSession implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) DeleteSession(ctx context.Context, req *agentv1.DeleteSessionReq) (resp *commonv1.OpResult, err error) {
	// TODO: Your code here...
	return
}

// ListMessages implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) ListMessages(ctx context.Context, req *agentv1.ListMessagesReq) (resp *agentv1.ListMessagesResp, err error) {
	// TODO: Your code here...
	return
}

// Chat implements the AgentServiceImpl interface.
func (s *AgentServiceImpl) Chat(ctx context.Context, req *agentv1.ChatReq) (resp *agentv1.ChatResp, err error) {
	// TODO: Your code here...
	return
}

func (s *AgentServiceImpl) ChatStream(req *agentv1.ChatReq, stream agentv1.AgentService_ChatStreamServer) (err error) {
	println("ChatStream called")
	return
}
