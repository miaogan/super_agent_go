package service

import (
	"context"

	agentv1 "github.com/aegis/idl/kitex_gen/agentv1"

	"github.com/aegis/pkg/identity"
	"github.com/aegis/pkg/logger"

	"github.com/aegis/agent_service/internal/orchestrate"
)

// ChatStream 流式对话。
//
// 与同步 Chat 的差别只在响应形态，编排逻辑完全复用 ——
// 这是刻意的：两份编排逻辑分叉会导致
// "流式能看到 RAG 资料、非流式看不到"这类极难定位的不一致。
//
// 事件顺序：session → retrieval → citation* → delta* → done
func (s *AgentService) ChatStream(req *agentv1.ChatReq, stream agentv1.AgentService_ChatStreamServer) error {
	ctx := identity.With(context.Background(), req.GetIdentity())

	if req.GetContent() == "" {
		return s.sendError(ctx, stream, "INVALID_PARAM", "content 不能为空")
	}

	agent, sessionID, err := s.resolve(ctx, req)
	if err != nil {
		// ★ 参数/鉴权类错误走 error 事件而非直接返回 error。
		//
		//   流式协议一旦建立就不能再改HTTP 状态码了，
		//   直接返回 error 会让客户端收到一个"没有内容类型"的裸错误，
		//   前端只能显示"未知错误"。发 error 事件才能带上明确原因。
		return s.sendError(ctx, stream, "REJECTED", err.Error())
	}

	err = s.orch.ChatStream(ctx, agent, req, sessionID, identity.From(ctx),
		func(ev *orchestrate.StreamEvent) bool {
			if sendErr := stream.Send(toChatEvent(ev)); sendErr != nil {
				// 客户端已断开。返回 false 让编排层停止生成 ——
				// 继续生成会白白烧掉模型 token。
				logger.Debug(ctx, "SSE 客户端断开，停止生成", "session_id", sessionID)
				return false
			}
			return true
		})

	if err != nil {
		logger.Error(ctx, "chat stream 失败", "session_id", sessionID, "err", err)
		return s.sendError(ctx, stream, "STREAM_FAILED", err.Error())
	}
	return nil
}

// toChatEvent 编排事件 → proto 事件
func toChatEvent(ev *orchestrate.StreamEvent) *agentv1.ChatEvent {
	return &agentv1.ChatEvent{
		Type:         ev.Type,
		Delta:        ev.Delta,
		SessionId:    ev.SessionID,
		MsgId:        ev.MsgID,
		Citation:     ev.Citation,
		HitCount:     ev.HitCount,
		RetrieveMs:   ev.RetrieveMS,
		LlmMs:        ev.LLMMS,
		Message:      ev.Message,
		Usage:        ev.Usage,
		Degradation:  ev.Degraded,
		ErrorCode:    ev.ErrorCode,
		ErrorMessage: ev.ErrorMsg,
	}
}

// sendError 发送 error 事件
func (s *AgentService) sendError(ctx context.Context, stream agentv1.AgentService_ChatStreamServer, code, msg string) error {
	if err := stream.Send(&agentv1.ChatEvent{
		Type:         orchestrate.EventError,
		ErrorCode:    code,
		ErrorMessage: msg,
	}); err != nil {
		return err
	}
	return nil
}
