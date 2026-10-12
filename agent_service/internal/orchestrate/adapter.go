package orchestrate

import (
	"context"

	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"
	llmservice "github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"
	ragservice "github.com/aegis/idl/kitex_gen/ragv1/ragservice"
)

// Kitex 生成的 client 方法都带 `...callopt.Option` 变参，
// 与编排层定义的窄接口签名不匹配。
// 这里的薄适配器把变参吃掉，顺便把「Kitex 细节」隔离在编排层之外，
// 使编排层可以用纯内存替身测试，无需起真实服务。

// LLMAdapter 把 llmservice.Client 适配为 ChatClient
type LLMAdapter struct {
	C llmservice.Client
}

var _ ChatClient = (*LLMAdapter)(nil)

// Chat 调用模型
func (a *LLMAdapter) Chat(ctx context.Context, req *llmv1.ChatReq) (*llmv1.ChatResp, error) {
	return a.C.Chat(ctx, req)
}

// ChatStream 流式调用模型
func (a *LLMAdapter) ChatStream(ctx context.Context, req *llmv1.ChatReq) (llmservice.LLMService_ChatStreamClient, error) {
	return a.C.ChatStream(ctx, req)
}

// RAGAdapter 把 ragservice.Client 适配为 RAGRetriever
type RAGAdapter struct {
	C ragservice.Client
}

var _ RAGRetriever = (*RAGAdapter)(nil)
var _ AgentKBMounter = (*RAGAdapter)(nil)

// Retrieve 执行检索
func (a *RAGAdapter) Retrieve(ctx context.Context, req *ragv1.RetrieveReq) (*ragv1.RetrieveResp, error) {
	return a.C.Retrieve(ctx, req)
}

// ListAgentKB 拉取 agent 绑定的知识库 ID 列表
func (a *RAGAdapter) ListAgentKB(ctx context.Context, agentID int64) ([]int64, error) {
	resp, err := a.C.ListAgentKB(ctx, &ragv1.ListAgentKBReq{AgentId: agentID})
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(resp.GetKbs()))
	for _, kb := range resp.GetKbs() {
		ids = append(ids, kb.GetKbId())
	}
	return ids, nil
}
