package service

import (
	"context"
	"testing"

	agentv1 "github.com/aegis/idl/kitex_gen/agentv1"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"
	"github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/identity"
	"github.com/aegis/pkg/mockx"

	"github.com/aegis/agent_service/internal/orchestrate"
	"github.com/aegis/agent_service/internal/store"
)

// ---------- 替身 ----------

type stubLLM struct{}

func (stubLLM) Chat(ctx context.Context, req *llmv1.ChatReq) (*llmv1.ChatResp, error) {
	return &llmv1.ChatResp{Content: "模型回答", Usage: &llmv1.Usage{TotalTokens: 10}}, nil
}

func (stubLLM) ChatStream(ctx context.Context, req *llmv1.ChatReq) (llmservice.LLMService_ChatStreamClient, error) {
	return nil, nil
}

type stubRAG struct{}

func (stubRAG) Retrieve(ctx context.Context, req *ragv1.RetrieveReq) (*ragv1.RetrieveResp, error) {
	return &ragv1.RetrieveResp{}, nil
}

// ---------- 夹具 ----------

type svcFixture struct {
	svc *AgentService
	st  *store.Store
}

func newSvcFixture(t *testing.T) *svcFixture {
	t.Helper()
	st := store.New(store.Options{})
	orch := orchestrate.New(orchestrate.Options{
		LLM:       stubLLM{},
		RAG:       stubRAG{},
		Store:     st,
		Agent:     conf.AgentConf{DefaultModel: "m", DefaultSystemPrompt: "你是助手"},
		RAGCfg:    conf.RAGConf{},
		EnableRAG: false,
	})
	svc := New(st, orch, mockx.NewFaultInjector(false, 0, nil))
	return &svcFixture{svc: svc, st: st}
}

func ctxWith(uid int64, scopes ...string) context.Context {
	return identity.With(context.Background(), &commonv1.Identity{
		Uid: uid, Username: "u", Tenant: "t1", Scopes: scopes,
	})
}

// createAgent 便捷创建：返回 agentID。
func (f *svcFixture) createAgent(t *testing.T, ctx context.Context, name string) int64 {
	t.Helper()
	resp, err := f.svc.CreateAgent(ctx, &agentv1.CreateAgentReq{Agent: &agentv1.Agent{
		Name: name, Model: "m", Status: store.AgentStatusEnabled,
	}})
	if err != nil {
		t.Fatalf("CreateAgent(%s) 失败: %v", name, err)
	}
	return resp.GetAgentId()
}

// ---------- Agent CRUD ----------

// TestCreateAgentSuccess 正常创建并回填身份。
func TestCreateAgentSuccess(t *testing.T) {
	f := newSvcFixture(t)
	id := f.createAgent(t, ctxWith(1001, "user"), "助手A")

	got, err := f.st.GetAgent(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAgent 失败: %v", err)
	}
	if got.GetOwnerUid() != 1001 || got.GetTenant() != "t1" {
		t.Errorf("身份未回填: owner=%d tenant=%q", got.GetOwnerUid(), got.GetTenant())
	}
	if got.GetStatus() != store.AgentStatusEnabled {
		t.Errorf("默认应启用: %d", got.GetStatus())
	}
}

// TestCreateAgentNil 空 Agent → ErrInvalidParam。
func TestCreateAgentNil(t *testing.T) {
	f := newSvcFixture(t)
	_, err := f.svc.CreateAgent(ctxWith(1), &agentv1.CreateAgentReq{})
	if errno.CodeOf(err) != errno.ErrInvalidParam {
		t.Errorf("应返回 ErrInvalidParam, 实际 %v", err)
	}
	_, err = f.svc.CreateAgent(ctxWith(1), &agentv1.CreateAgentReq{Agent: &agentv1.Agent{}})
	if errno.CodeOf(err) != errno.ErrInvalidParam {
		t.Errorf("空 name 应返回 ErrInvalidParam, 实际 %v", err)
	}
}

// TestCreateAgentOwnerTrusted 请求体伪造 owner_uid 应被忽略，以身份为准。
func TestCreateAgentOwnerTrusted(t *testing.T) {
	f := newSvcFixture(t)
	resp, err := f.svc.CreateAgent(ctxWith(1001, "user"), &agentv1.CreateAgentReq{
		Agent: &agentv1.Agent{Name: "防伪造", OwnerUid: 99999, Tenant: "evil"},
	})
	if err != nil {
		t.Fatalf("CreateAgent 失败: %v", err)
	}
	got, _ := f.st.GetAgent(context.Background(), resp.GetAgentId())
	if got.GetOwnerUid() != 1001 || got.GetTenant() != "t1" {
		t.Errorf("身份必须取自 ctx: owner=%d tenant=%q", got.GetOwnerUid(), got.GetTenant())
	}
}

// TestGetAgentForbidden 他人 Agent → ErrForbidden。
func TestGetAgentForbidden(t *testing.T) {
	f := newSvcFixture(t)
	id := f.createAgent(t, ctxWith(1001, "user"), "私有Agent")

	_, err := f.svc.GetAgent(ctxWith(1002, "user"), &agentv1.GetAgentReq{AgentId: id})
	if errno.CodeOf(err) != errno.ErrForbidden {
		t.Errorf("他人 Agent 应 ErrForbidden, 实际 %v", err)
	}
	// admin 可读
	_, err = f.svc.GetAgent(ctxWith(1, "user", "admin"), &agentv1.GetAgentReq{AgentId: id})
	if err != nil {
		t.Errorf("admin 应可读: %v", err)
	}
}

// TestGetAgentPublic 公共 Agent（owner=0）任何登录用户可读。
func TestGetAgentPublic(t *testing.T) {
	f := newSvcFixture(t)
	// 直接造一个 owner=0 的公共 Agent（绕过 CreateAgent 的身份回填）
	id, err := f.st.CreateAgent(context.Background(), &agentv1.Agent{
		Name: "公共Agent", OwnerUid: 0, Tenant: "",
	})
	if err != nil {
		t.Fatalf("CreateAgent 失败: %v", err)
	}
	got, err := f.svc.GetAgent(ctxWith(777, "user"), &agentv1.GetAgentReq{AgentId: id})
	if err != nil {
		t.Fatalf("公共 Agent 任何登录用户应可读: %v", err)
	}
	if got.GetAgent().GetName() != "公共Agent" {
		t.Errorf("名称不符: %q", got.GetAgent().GetName())
	}
}

// TestUpdateAgentKeepsOwner 更新不能改写 owner/tenant。
func TestUpdateAgentKeepsOwner(t *testing.T) {
	f := newSvcFixture(t)
	id := f.createAgent(t, ctxWith(1001, "user"), "原名")

	_, err := f.svc.UpdateAgent(ctxWith(1001, "user"), &agentv1.UpdateAgentReq{
		Agent: &agentv1.Agent{
			AgentId: id, Name: "新名",
			OwnerUid: 555, Tenant: "evil",
		},
		UpdateMask: []string{"name"},
	})
	if err != nil {
		t.Fatalf("UpdateAgent 失败: %v", err)
	}
	got, _ := f.st.GetAgent(context.Background(), id)
	if got.GetName() != "新名" {
		t.Errorf("名称应更新: %q", got.GetName())
	}
	if got.GetOwnerUid() != 1001 || got.GetTenant() != "t1" {
		t.Errorf("owner/tenant 不可被改写: %d/%q", got.GetOwnerUid(), got.GetTenant())
	}
}

// TestUpdateAgentForbidden 他人 Agent 更新 → ErrForbidden。
func TestUpdateAgentForbidden(t *testing.T) {
	f := newSvcFixture(t)
	id := f.createAgent(t, ctxWith(1001, "user"), "私有")

	_, err := f.svc.UpdateAgent(ctxWith(1002, "user"), &agentv1.UpdateAgentReq{
		Agent: &agentv1.Agent{AgentId: id, Name: "x"}, UpdateMask: []string{"name"},
	})
	if errno.CodeOf(err) != errno.ErrForbidden {
		t.Errorf("应 ErrForbidden, 实际 %v", err)
	}
}

// TestListAgentsScope 非 admin 强制只看自己；admin 可跨租户。
func TestListAgentsScope(t *testing.T) {
	f := newSvcFixture(t)
	f.createAgent(t, ctxWith(1001, "user"), "我的Agent")
	f.createAgent(t, ctxWith(2001, "user"), "别人的Agent")

	// 非 admin 显式请求 owner_uid=2001 → 被降级为只看自己
	resp, err := f.svc.ListAgents(ctxWith(1001, "user"), &agentv1.ListAgentsReq{OwnerUid: 2001})
	if err != nil {
		t.Fatalf("ListAgents 失败: %v", err)
	}
	if len(resp.GetAgents()) != 1 {
		t.Errorf("非 admin 应只看自己的 1 条, 实际 %d", len(resp.GetAgents()))
	}

	// admin 可见全部
	adminResp, err := f.svc.ListAgents(ctxWith(1, "user", "admin"), &agentv1.ListAgentsReq{})
	if err != nil {
		t.Fatalf("ListAgents(admin) 失败: %v", err)
	}
	if int(adminResp.GetMeta().GetTotal()) < 2 {
		t.Errorf("admin 应可见全部, 实际 %d", adminResp.GetMeta().GetTotal())
	}
}

// TestDeleteAgentForbidden 他人 Agent 删除 → ErrForbidden。
func TestDeleteAgentForbidden(t *testing.T) {
	f := newSvcFixture(t)
	id := f.createAgent(t, ctxWith(1001, "user"), "私有")

	_, err := f.svc.DeleteAgent(ctxWith(1002, "user"), &agentv1.DeleteAgentReq{AgentId: id})
	if errno.CodeOf(err) != errno.ErrForbidden {
		t.Errorf("应 ErrForbidden, 实际 %v", err)
	}
}

// TestMapErr 仓储错误映射。
func TestMapErr(t *testing.T) {
	if err := mapErr(nil); err != nil {
		t.Errorf("mapErr(nil) 应为 nil: %v", err)
	}
	if errno.CodeOf(mapErr(&store.ErrNotFound{})) != errno.ErrNotFound {
		t.Error("NotFound 应映射 ErrNotFound")
	}
	if errno.CodeOf(mapErr(&store.ErrConflict{})) != errno.ErrConflict {

		t.Error("Conflict 应映射 ErrConflict")
	}
	// 重名要映射到 ErrAlreadyExists（"资源已存在"），落到 ErrConflict 会显示
	// "当前状态不允许该操作"，用户看不出自己只是名字重了。
	if errno.CodeOf(mapErr(&store.ErrDuplicate{Msg: "同名 Agent 已存在: x"})) != errno.ErrAlreadyExists {
		t.Error("Duplicate 应映射 ErrAlreadyExists")
	}
	if errno.CodeOf(mapErr(&store.ErrForbidden{})) != errno.ErrForbidden {
		t.Error("Forbidden 应映射 ErrForbidden")
	}
	if errno.CodeOf(mapErr(context.DeadlineExceeded)) != errno.ErrInternal {
		t.Error("其他错误应映射 ErrInternal")
	}
}

// ---------- Session / Chat ----------

// TestCreateSession 为他人 Agent 建会话 → ErrForbidden。
func TestCreateSessionForbidden(t *testing.T) {
	f := newSvcFixture(t)
	id := f.createAgent(t, ctxWith(1001, "user"), "私有")

	_, err := f.svc.CreateSession(ctxWith(1002, "user"), &agentv1.CreateSessionReq{AgentId: id})
	if errno.CodeOf(err) != errno.ErrForbidden {
		t.Errorf("应 ErrForbidden, 实际 %v", err)
	}
}

// TestChatAutoCreateSession session_id=0 时自动建会话。
func TestChatAutoCreateSession(t *testing.T) {
	f := newSvcFixture(t)
	id := f.createAgent(t, ctxWith(1001, "user"), "对话Agent")

	resp, err := f.svc.Chat(ctxWith(1001, "user"), &agentv1.ChatReq{
		AgentId: id, Content: "你好",
	})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if resp.GetSessionId() == 0 {
		t.Error("应自动创建会话")
	}
	if resp.GetMessage().GetContent() == "" {
		t.Error("应返回模型回答")
	}
}

// TestChatDisabledAgent 停用的 Agent 不可对话 → ErrForbidden。
func TestChatDisabledAgent(t *testing.T) {
	f := newSvcFixture(t)
	id := f.createAgent(t, ctxWith(1001, "user"), "停用Agent")
	f.st.UpdateAgent(context.Background(), &agentv1.Agent{
		AgentId: id, Status: store.AgentStatusDisabled,
	}, []string{"status"})

	_, err := f.svc.Chat(ctxWith(1001, "user"), &agentv1.ChatReq{AgentId: id, Content: "hi"})
	if errno.CodeOf(err) != errno.ErrForbidden {
		t.Errorf("停用 Agent 应 ErrForbidden, 实际 %v", err)
	}
}

// TestChatEmptyContent 空内容 → ErrInvalidParam。
func TestChatEmptyContent(t *testing.T) {
	f := newSvcFixture(t)
	_, err := f.svc.Chat(ctxWith(1001, "user"), &agentv1.ChatReq{AgentId: 1, Content: ""})
	if errno.CodeOf(err) != errno.ErrInvalidParam {
		t.Errorf("应 ErrInvalidParam, 实际 %v", err)
	}
}

// TestChatSessionMismatch 会话不属于该 Agent → ErrInvalidParam。
func TestChatSessionMismatch(t *testing.T) {
	f := newSvcFixture(t)
	idA := f.createAgent(t, ctxWith(1001, "user"), "AgentA")
	idB := f.createAgent(t, ctxWith(1001, "user"), "AgentB")

	sess, err := f.st.CreateSession(context.Background(), idA, "", &commonv1.Identity{Uid: 1001})
	if err != nil {
		t.Fatalf("CreateSession 失败: %v", err)
	}
	_, err = f.svc.Chat(ctxWith(1001, "user"), &agentv1.ChatReq{
		AgentId: idB, SessionId: sess.GetSessionId(), Content: "hi",
	})
	if errno.CodeOf(err) != errno.ErrInvalidParam {
		t.Errorf("会话归属不符应 ErrInvalidParam, 实际 %v", err)
	}
}

// TestCheckSessionOwner 他人会话 → ErrForbidden。
func TestCheckSessionOwner(t *testing.T) {
	f := newSvcFixture(t)
	id := f.createAgent(t, ctxWith(1001, "user"), "A")
	sess, _ := f.st.CreateSession(context.Background(), id, "", &commonv1.Identity{Uid: 1001})

	_, err := f.svc.GetSession(ctxWith(1002, "user"), &agentv1.GetSessionReq{SessionId: sess.GetSessionId()})
	if errno.CodeOf(err) != errno.ErrForbidden {
		t.Errorf("他人会话应 ErrForbidden, 实际 %v", err)
	}
}
