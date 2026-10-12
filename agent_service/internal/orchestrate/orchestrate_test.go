package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	agentv1 "github.com/aegis/idl/kitex_gen/agentv1"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"
	"github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"
	"github.com/cloudwego/kitex/pkg/remote/trans/nphttp2/metadata"

	"github.com/aegis/pkg/conf"
)

// ---------- 替身 ----------

// fakeLLM 模型调用替身
type fakeLLM struct {
	mu        sync.Mutex
	resp      *llmv1.ChatResp
	err       error
	stream    []streamFrame
	streamErr error

	gotChat   *llmv1.ChatReq
	gotStream *llmv1.ChatReq
}

type streamFrame struct {
	chunk *llmv1.ChatChunk
	err   error
}

func (f *fakeLLM) Chat(ctx context.Context, req *llmv1.ChatReq) (*llmv1.ChatResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotChat = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakeLLM) ChatStream(ctx context.Context, req *llmv1.ChatReq) (llmservice.LLMService_ChatStreamClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotStream = req
	if f.err != nil {
		return nil, f.err
	}
	return &fakeStream{frames: f.stream}, nil
}

func (f *fakeLLM) chatReq() *llmv1.ChatReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotChat
}

func (f *fakeLLM) streamReq() *llmv1.ChatReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotStream
}

// fakeStream 实现 llmservice.LLMService_ChatStreamClient
type fakeStream struct {
	mu     sync.Mutex
	frames []streamFrame
	i      int
	md     metadata.MD
}

func (s *fakeStream) Recv() (*llmv1.ChatChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.i >= len(s.frames) {
		return nil, io.EOF
	}
	f := s.frames[s.i]
	s.i++
	if f.err != nil {
		return nil, f.err
	}
	return f.chunk, nil
}

func (s *fakeStream) SetHeader(md metadata.MD) error { s.md = md; return nil }
func (s *fakeStream) SendHeader(md metadata.MD) error {
	s.md = metadata.Join(s.md, md)
	return nil
}
func (s *fakeStream) Header() (metadata.MD, error) { return s.md, nil }
func (s *fakeStream) SetTrailer(md metadata.MD)    {}
func (s *fakeStream) Trailer() metadata.MD         { return nil }
func (s *fakeStream) Context() context.Context     { return context.Background() }
func (s *fakeStream) SendMsg(m interface{}) error  { return nil }
func (s *fakeStream) RecvMsg(m interface{}) error  { return nil }
func (s *fakeStream) Close() error                 { return nil }

// 编译期断言：替身必须真的满足流式接口
var _ llmservice.LLMService_ChatStreamClient = (*fakeStream)(nil)

// fakeRAG 检索替身
type fakeRAG struct {
	mu    sync.Mutex
	resp  *ragv1.RetrieveResp
	err   error
	calls int
	got   *ragv1.RetrieveReq
}

func (f *fakeRAG) Retrieve(ctx context.Context, req *ragv1.RetrieveReq) (*ragv1.RetrieveResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakeRAG) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRAG) lastReq() *ragv1.RetrieveReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got
}

// fakeStore 会话存储替身
type fakeStore struct {
	mu        sync.Mutex
	recent    []*agentv1.ChatMessage
	historyN  int // 记录被请求的历史条数
	appended  []*agentv1.ChatMessage
	appendErr error
	nextMsgID int64
}

func (f *fakeStore) RecentMessages(ctx context.Context, sessionID int64, n int) []*agentv1.ChatMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.historyN = n
	return f.recent
}

func (f *fakeStore) AppendMessage(ctx context.Context, sessionID int64, msg *agentv1.ChatMessage) (*agentv1.ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendErr != nil {
		return nil, f.appendErr
	}
	f.nextMsgID++
	cp := *msg
	cp.MsgId = f.nextMsgID
	f.appended = append(f.appended, &cp)
	return &cp, nil
}

func (f *fakeStore) appendedMsgs() []*agentv1.ChatMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appended
}

// ---------- 夹具 ----------

func testAgentCfg() conf.AgentConf {
	return conf.AgentConf{
		DefaultModel:          "global-model",
		DefaultTemperature:    0.7,
		DefaultMaxTokens:      1024,
		DefaultTopK:           3,
		DefaultSystemPrompt:   "你是全局助手",
		HistoryTurns:          5,
		MaxContextChars:       1000,
		MaxMessagesPerSession: 100,
	}
}

func testRAGCfg() conf.RAGConf {
	return conf.RAGConf{
		DefaultTopK:           4,
		DefaultScoreThreshold: 0.3,
		ContextWindow:         1,
	}
}

func okLLM() *fakeLLM {
	return &fakeLLM{resp: &llmv1.ChatResp{
		Content: "模型回答",
		Usage:   &llmv1.Usage{PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33},
	}}
}

func newOrch(llm ChatClient, rag RAGRetriever, st SessionStore) *Orchestrator {
	return New(Options{
		LLM:       llm,
		RAG:       rag,
		Store:     st,
		Agent:     testAgentCfg(),
		RAGCfg:    testRAGCfg(),
		EnableRAG: true,
	})
}

func testIdentity() *commonv1.Identity {
	return &commonv1.Identity{Uid: 1001, Tenant: "t1", Username: "u1"}
}

func hit(chunkID string, kbID, docID int64, title string, page int32, score float64, content string) *ragv1.Hit {
	return &ragv1.Hit{
		ChunkId:  chunkID,
		KbId:     kbID,
		DocId:    docID,
		DocTitle: title,
		PageNo:   page,
		Score:    score,
		Content:  content,
	}
}

// ---------- PlanFrom：优先级 ----------

func TestPlanFromUsesGlobalDefaults(t *testing.T) {
	p := PlanFrom(nil, &agentv1.ChatReq{Content: "hi"}, testAgentCfg(), testRAGCfg())
	if p.Model != "global-model" {
		t.Errorf("Model = %q, 期望 global-model", p.Model)
	}
	if p.Temperature != 0.7 {
		t.Errorf("Temperature = %v, 期望 0.7", p.Temperature)
	}
	if p.MaxTokens != 1024 {
		t.Errorf("MaxTokens = %d, 期望 1024", p.MaxTokens)
	}
	if p.SystemPrompt != "你是全局助手" {
		t.Errorf("SystemPrompt = %q", p.SystemPrompt)
	}
	if p.UseRAG {
		t.Error("没有 Agent 也没有请求指定时，UseRAG 应为 false")
	}
	if p.TopK != 4 {
		t.Errorf("TopK = %d, 期望取 ragCfg.DefaultTopK=4", p.TopK)
	}
}

func TestPlanFromAgentOverridesGlobal(t *testing.T) {
	a := &agentv1.Agent{
		Model:             "agent-model",
		Temperature:       0.2,
		MaxTokens:         256,
		RagTopK:           9,
		RagScoreThreshold: 0.8,
		RagEnabled:        true,
		KbIds:             []int64{7},
		Tools:             []string{"calc"},
		SystemPrompt:      "你是专用助手",
	}
	p := PlanFrom(a, &agentv1.ChatReq{Content: "hi"}, testAgentCfg(), testRAGCfg())
	if p.Model != "agent-model" || p.Temperature != 0.2 || p.MaxTokens != 256 {
		t.Errorf("Agent 配置未覆盖全局: %+v", p)
	}
	if p.TopK != 9 || p.ScoreThreshold != 0.8 {
		t.Errorf("RAG 参数未覆盖: topK=%d thr=%v", p.TopK, p.ScoreThreshold)
	}
	if !p.UseRAG {
		t.Error("UseRAG 应为 true")
	}
	if p.SystemPrompt != "你是专用助手" {
		t.Errorf("SystemPrompt = %q", p.SystemPrompt)
	}
	if len(p.Tools) != 1 || p.Tools[0] != "calc" {
		t.Errorf("Tools = %v", p.Tools)
	}
}

func TestPlanFromRequestOverridesAgent(t *testing.T) {
	a := &agentv1.Agent{Model: "agent-model", MaxTokens: 256, RagEnabled: true, KbIds: []int64{7}}
	// agentv1.ChatReq 只暴露 model / kb_ids / use_rag 三个覆盖项，
	// temperature 与 max_tokens 不允许请求侧改（避免调用方绕过 Agent 的成本约束）
	req := &agentv1.ChatReq{Content: "hi", Model: "req-model", KbIds: []int64{8, 9}}
	p := PlanFrom(a, req, testAgentCfg(), testRAGCfg())
	if p.Model != "req-model" {
		t.Errorf("请求应覆盖 Agent: Model = %q", p.Model)
	}
	if p.MaxTokens != 256 {
		t.Errorf("MaxTokens 应沿用 Agent 的 256，实际 %d", p.MaxTokens)
	}
	// 显式指定知识库即视为要检索，即便 Agent.RagEnabled 为 false
	if len(p.KbIDs) != 2 || p.KbIDs[0] != 8 {
		t.Errorf("KbIDs = %v", p.KbIDs)
	}
	if !p.UseRAG {
		t.Error("显式传 kb_ids 应隐式开启 RAG")
	}
}

func TestPlanFromRequestUseRagForcesOn(t *testing.T) {
	// Agent 明确关了 RAG，请求 use_rag=true 应能重新打开
	a := &agentv1.Agent{RagEnabled: false, KbIds: []int64{7}}
	p := PlanFrom(a, &agentv1.ChatReq{Content: "hi", UseRag: true}, testAgentCfg(), testRAGCfg())
	if !p.UseRAG {
		t.Error("请求 use_rag=true 应覆盖 Agent 的关闭")
	}
}

func TestPlanFromKeepsAgentRagOffWhenNotRequested(t *testing.T) {
	a := &agentv1.Agent{RagEnabled: false, KbIds: []int64{7}}
	p := PlanFrom(a, &agentv1.ChatReq{Content: "hi"}, testAgentCfg(), testRAGCfg())
	if p.UseRAG {
		t.Error("请求未指定时，不应擅自打开 Agent 关闭的 RAG")
	}
}

func TestPlanFromTopKFallback(t *testing.T) {
	// Agent.RagTopK = 0 时应回落到 ragCfg.DefaultTopK，不能留 0
	a := &agentv1.Agent{RagEnabled: true, KbIds: []int64{1}}
	p := PlanFrom(a, &agentv1.ChatReq{Content: "hi"}, testAgentCfg(), testRAGCfg())
	if p.TopK != 4 {
		t.Errorf("TopK = %d, 期望回落到 4", p.TopK)
	}
}

func TestPlanFromNilAgentAndNilReqSafe(t *testing.T) {
	// 都为 nil 不应 panic —— PlanFrom 在单测和降级路径都会被这么调
	p := PlanFrom(nil, nil, testAgentCfg(), testRAGCfg())
	if p.Model != "global-model" {
		t.Errorf("Model = %q", p.Model)
	}
}

// ---------- Retrieve：RAG 永不阻断 ----------

func TestRetrieveNeverReturnsError(t *testing.T) {
	rag := &fakeRAG{err: errors.New("milvus down")}
	o := newOrch(okLLM(), rag, &fakeStore{})
	plan := AgentPlan{UseRAG: true, KbIDs: []int64{1}, TopK: 3, MaxContextChars: 500}

	rc := o.Retrieve(context.Background(), "问题", plan, testIdentity(), "rid")

	if !rc.Degraded {
		t.Error("检索失败必须标记 Degraded")
	}
	if !strings.Contains(rc.Reason, "milvus down") {
		t.Errorf("降级原因应透传底层错误，实际 = %q", rc.Reason)
	}
	if len(rc.Citations) != 0 {
		t.Errorf("失败时不应有引用，实际 %d 条", len(rc.Citations))
	}
}

func TestRetrieveSkippedWhenDisabled(t *testing.T) {
	rag := &fakeRAG{err: errors.New("不应被调用")}
	o := newOrch(okLLM(), rag, &fakeStore{})

	// 三种跳过条件各测一次
	for name, plan := range map[string]AgentPlan{
		"RAG关闭": {UseRAG: false, KbIDs: []int64{1}},
		"无知识库":  {UseRAG: true, KbIDs: nil},
		"空查询":   {UseRAG: true, KbIDs: []int64{1}},
	} {
		query := "问题"
		if name == "空查询" {
			query = "   "
		}
		rc := o.Retrieve(context.Background(), query, plan, testIdentity(), "rid")
		if rc.Degraded || rc.HitCount != 0 {
			t.Errorf("%s: 不应检索也不应降级，实际 %+v", name, rc)
		}
	}
	if rag.callCount() != 0 {
		t.Errorf("跳过路径不应真的调用下游，调用了 %d 次", rag.callCount())
	}
}

func TestRetrieveNilRAGSafe(t *testing.T) {
	o := newOrch(okLLM(), nil, &fakeStore{})
	plan := AgentPlan{UseRAG: true, KbIDs: []int64{1}, MaxContextChars: 500}
	rc := o.Retrieve(context.Background(), "问题", plan, testIdentity(), "rid")
	if rc.Degraded || rc.HitCount != 0 {
		t.Errorf("RAG 为 nil 时应静默跳过，实际 %+v", rc)
	}
}

func TestRetrieveBuildsNumberedCitations(t *testing.T) {
	rag := &fakeRAG{resp: &ragv1.RetrieveResp{Hits: []*ragv1.Hit{
		hit("c1", 1, 11, "合同.pdf", 3, 0.9, "第一条 内容"),
		hit("c2", 1, 12, "手册.docx", 0, 0.8, "第二条 内容"),
	}}}
	o := newOrch(okLLM(), rag, &fakeStore{})
	plan := AgentPlan{UseRAG: true, KbIDs: []int64{1}, TopK: 2, MaxContextChars: 500}

	rc := o.Retrieve(context.Background(), "问题", plan, testIdentity(), "rid")

	if rc.HitCount != 2 {
		t.Fatalf("HitCount = %d, 期望 2", rc.HitCount)
	}
	if len(rc.Citations) != 2 {
		t.Fatalf("引用数 = %d, 期望 2", len(rc.Citations))
	}
	// 编号必须从 1 递增，与模型回答里的 [n] 严格对应
	for i, c := range rc.Citations {
		if c.Index != int32(i+1) {
			t.Errorf("第 %d 条引用编号 = %d, 期望 %d", i, c.Index, i+1)
		}
	}
	if rc.Citations[0].DocId != 11 || rc.Citations[0].PageNo != 3 {
		t.Errorf("引用未正确映射回源文档: %+v", rc.Citations[0])
	}
	if !strings.Contains(rc.ContextText, "[1] 来自《合同.pdf》 第3页") {
		t.Errorf("上下文缺少页码标注:\n%s", rc.ContextText)
	}
	if strings.Contains(rc.ContextText, "第0页") {
		t.Errorf("page_no=0 不应渲染成「第0页」:\n%s", rc.ContextText)
	}
	if rag.lastReq().GetQuery() != "问题" {
		t.Error("检索 query 未透传")
	}
}

func TestRetrieveSkipsEmptyContentHits(t *testing.T) {
	rag := &fakeRAG{resp: &ragv1.RetrieveResp{Hits: []*ragv1.Hit{
		hit("c1", 1, 11, "空.pdf", 0, 0.9, "   "),
		hit("c2", 1, 12, "实.pdf", 0, 0.8, "有内容"),
	}}}
	o := newOrch(okLLM(), rag, &fakeStore{})
	rc := o.Retrieve(context.Background(), "问题",
		AgentPlan{UseRAG: true, KbIDs: []int64{1}, MaxContextChars: 500},
		testIdentity(), "rid")

	// HitCount 统计原始命中，引用只建有正文的
	if rc.HitCount != 2 {
		t.Errorf("HitCount 应统计原始命中数，实际 %d", rc.HitCount)
	}
	if len(rc.Citations) != 1 {
		t.Fatalf("空内容不应建引用，实际 %d 条", len(rc.Citations))
	}
	if rc.Citations[0].DocTitle != "实.pdf" {
		t.Errorf("引用错位: %+v", rc.Citations[0])
	}
}

func TestRetrieveRespectsCharBudget(t *testing.T) {
	long := strings.Repeat("长", 800)
	rag := &fakeRAG{resp: &ragv1.RetrieveResp{Hits: []*ragv1.Hit{
		hit("c1", 1, 11, "长文.pdf", 0, 0.9, long),
		hit("c2", 1, 12, "次文.pdf", 0, 0.8, long),
	}}}
	o := newOrch(okLLM(), rag, &fakeStore{})
	// 预算 100：第一条就被截断，第二条因预算耗尽应 break
	rc := o.Retrieve(context.Background(), "问题",
		AgentPlan{UseRAG: true, KbIDs: []int64{1}, MaxContextChars: 100},
		testIdentity(), "rid")

	if len(rc.Citations) != 1 {
		t.Fatalf("预算耗尽后应停止建引用，实际 %d 条", len(rc.Citations))
	}
	if !strings.Contains(rc.ContextText, "…") {
		t.Error("超长切片应被截断并加省略号")
	}
	if strings.Contains(rc.ContextText, "次文") {
		t.Error("预算耗尽后不应再拼接后续切片")
	}
}

func TestRetrieveDefaultBudgetWhenUnset(t *testing.T) {
	rag := &fakeRAG{resp: &ragv1.RetrieveResp{Hits: []*ragv1.Hit{
		hit("c1", 1, 11, "a.pdf", 0, 0.9, strings.Repeat("x", 5000)),
	}}}
	o := newOrch(okLLM(), rag, &fakeStore{})
	// MaxContextChars = 0 → 应回落 4000 默认值，而不是不截断
	rc := o.Retrieve(context.Background(), "问题",
		AgentPlan{UseRAG: true, KbIDs: []int64{1}, MaxContextChars: 0},
		testIdentity(), "rid")
	if len(rc.Citations) != 1 {
		t.Fatalf("默认预算下应仍能建引用，实际 %d 条", len(rc.Citations))
	}
	if len(rc.ContextText) > 4200 {
		t.Errorf("默认预算未生效，上下文长度 %d", len(rc.ContextText))
	}
}

func TestRetrievePropagatesDownstreamDegradation(t *testing.T) {
	// 下游自己降级（Milvus 不可用走 PG 关键词），本层必须如实透传
	rag := &fakeRAG{resp: &ragv1.RetrieveResp{
		Hits:          []*ragv1.Hit{hit("c1", 1, 11, "a.pdf", 0, 0.5, "内容")},
		Degraded:      true,
		DegradeReason: "milvus 不可用，已降级为关键词检索",
	}}
	o := newOrch(okLLM(), rag, &fakeStore{})
	rc := o.Retrieve(context.Background(), "问题",
		AgentPlan{UseRAG: true, KbIDs: []int64{1}, MaxContextChars: 500},
		testIdentity(), "rid")

	if !rc.Degraded {
		t.Error("下游降级标记未透传")
	}
	if rc.Reason != "milvus 不可用，已降级为关键词检索" {
		t.Errorf("降级原因 = %q", rc.Reason)
	}
}

// ---------- BuildMessages ----------

func TestBuildMessagesPutsRAGInSystem(t *testing.T) {
	o := newOrch(okLLM(), nil, &fakeStore{})
	plan := AgentPlan{SystemPrompt: "你是助手"}
	rc := RetrievalContext{ContextText: "[1] 来自《a.pdf》（相关度 0.90）：\n内容"}

	msgs := o.BuildMessages(plan, "问题", rc, nil)

	if len(msgs) == 0 || msgs[0].Role != "system" {
		t.Fatalf("首条应为 system，实际 %+v", msgs)
	}
	if !strings.Contains(msgs[0].Content, "内容") {
		t.Error("RAG 资料应注入 system 消息")
	}
	if !strings.Contains(msgs[0].Content, "你是助手") {
		t.Error("原始 system 提示词丢失")
	}
	// 关键：资料不能塞进 user 消息，否则模型会当成用户的话去回应
	last := msgs[len(msgs)-1]
	if last.Role != "user" || last.Content != "问题" {
		t.Errorf("末条应为纯 user 问题，实际 %+v", last)
	}
}

func TestBuildMessagesOrder(t *testing.T) {
	o := newOrch(okLLM(), nil, &fakeStore{})
	history := []*agentv1.ChatMessage{
		{Role: "user", Content: "历史问题1"},
		{Role: "assistant", Content: "历史回答1"},
		{Role: "tool", Content: "工具输出"},  // ★ 现在应参与 ReAct 循环
		{Role: "assistant", Content: ""},     // 仍被过滤（空 content 且非 tool 消息）
		{Role: "user", Content: "历史问题2"},
	}
	msgs := o.BuildMessages(AgentPlan{SystemPrompt: "sys"}, "本轮问题", RetrievalContext{}, history)

	// ★ tool 消息现在参与（ReAct 循环需要 tool result 回喂 LLM）
	wantRoles := []string{"system", "user", "assistant", "tool", "user", "user"}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("消息数 = %d, 期望 %d: %+v", len(msgs), len(wantRoles), roles(msgs))
	}
	for i, want := range wantRoles {
		if msgs[i].Role != want {
			t.Errorf("第 %d 条 role = %q, 期望 %q", i, msgs[i].Role, want)
		}
	}
	if msgs[len(msgs)-1].Content != "本轮问题" {
		t.Error("本轮提问必须在最后")
	}
}

func TestBuildMessagesNoRAGKeepsSystemClean(t *testing.T) {
	o := newOrch(okLLM(), nil, &fakeStore{})
	msgs := o.BuildMessages(AgentPlan{SystemPrompt: "纯提示"}, "问题", RetrievalContext{}, nil)
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d, 期望 2", len(msgs))
	}
	if msgs[0].Content != "纯提示" {
		t.Errorf("无资料时 system 不应被追加内容: %q", msgs[0].Content)
	}
}

func TestBuildMessagesStreamAndSyncShareLogic(t *testing.T) {
	// 流式与非流式必须共用同一套拼装，否则会出现
	// "流式能看到资料、同步看不到"这类难查的不一致
	o := newOrch(okLLM(), nil, &fakeStore{})
	plan := AgentPlan{SystemPrompt: "sys"}
	rc := RetrievalContext{ContextText: "[1] x"}
	history := []*agentv1.ChatMessage{{Role: "user", Content: "h"}}

	a := o.BuildMessages(plan, "q", rc, history)
	b := o.BuildMessages(plan, "q", rc, history)
	if len(a) != len(b) {
		t.Fatalf("两次拼装结果长度不一致: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Role != b[i].Role || a[i].Content != b[i].Content {
			t.Errorf("第 %d 条不一致", i)
		}
	}
}

func roles(msgs []*llmv1.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Role)
	}
	return out
}

// ---------- Chat ----------

func TestChatSuccess(t *testing.T) {
	llm := okLLM()
	st := &fakeStore{}
	o := newOrch(llm, nil, st)

	res, err := o.Chat(context.Background(),
		&agentv1.Agent{Model: "m", MaxTokens: 100},
		&agentv1.ChatReq{Content: "问题", RequestId: "rid-1"},
		5001, testIdentity())
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if res.Message.GetContent() != "模型回答" {
		t.Errorf("内容 = %q", res.Message.GetContent())
	}
	if res.Message.GetMsgId() == 0 {
		t.Error("消息应已落库并拿到 msg_id")
	}
	if res.Message.GetUsage().GetTotalTokens() != 33 {
		t.Errorf("usage 未透传: %+v", res.Message.GetUsage())
	}
	// request_id 必须传给下游，否则无法排障
	if llm.chatReq().GetRequestId() != "rid-1" {
		t.Error("request_id 未透传给 llm_service")
	}
	if llm.chatReq().GetIdentity().GetUid() != 1001 {
		t.Error("identity 未透传给 llm_service")
	}
}

func TestChatRejectsEmptyContent(t *testing.T) {
	o := newOrch(okLLM(), nil, &fakeStore{})
	for _, c := range []string{"", "   ", "\n\t "} {
		_, err := o.Chat(context.Background(), nil,
			&agentv1.ChatReq{Content: c}, 1, testIdentity())
		if err == nil {
			t.Errorf("内容 %q 应被拒绝", c)
		}
	}
}

func TestChatSurvivesRAGFailure(t *testing.T) {
	llm := okLLM()
	rag := &fakeRAG{err: errors.New("vector store timeout")}
	st := &fakeStore{}
	o := newOrch(llm, rag, st)

	res, err := o.Chat(context.Background(),
		&agentv1.Agent{RagEnabled: true, KbIds: []int64{1}},
		&agentv1.ChatReq{Content: "问题"}, 1, testIdentity())

	if err != nil {
		t.Fatalf("RAG 故障不应让对话失败: %v", err)
	}
	if res.Message.GetContent() != "模型回答" {
		t.Error("降级后仍应返回模型回答")
	}
	// 但必须如实标记降级，不能静默假装有资料
	if !res.Message.GetDegraded() {
		t.Error("RAG 故障应在消息上标记 degraded")
	}
	if !strings.Contains(res.Message.GetDegradeReason(), "vector store timeout") {
		t.Errorf("降级原因 = %q", res.Message.GetDegradeReason())
	}
	if res.Degraded.GetDegraded() != true {
		t.Error("Result.Degraded 未设置")
	}
}

func TestChatLLMErrorPropagates(t *testing.T) {
	llm := &fakeLLM{err: errors.New("model overloaded")}
	o := newOrch(llm, nil, &fakeStore{})

	_, err := o.Chat(context.Background(), nil, &agentv1.ChatReq{Content: "问题"}, 1, testIdentity())
	if err == nil {
		t.Fatal("模型失败应返回错误")
	}
	if !strings.Contains(err.Error(), "model overloaded") {
		t.Errorf("错误信息应含底层原因: %v", err)
	}
}

func TestChatReturnsAnswerEvenIfPersistFails(t *testing.T) {
	st := &fakeStore{appendErr: errors.New("pg down")}
	o := newOrch(okLLM(), nil, st)

	res, err := o.Chat(context.Background(), nil, &agentv1.ChatReq{Content: "问题"}, 1, testIdentity())
	if err != nil {
		t.Fatalf("落库失败不应让用户丢掉已生成的回答: %v", err)
	}
	// ★ 绝不返回空响应 —— 用户会以为回答是空的
	if res.Message.GetContent() != "模型回答" {
		t.Errorf("落库失败后内容丢失: %+v", res.Message)
	}
}

func TestChatHistoryFetchedAfterRetrieve(t *testing.T) {
	st := &fakeStore{recent: []*agentv1.ChatMessage{{Role: "user", Content: "旧问题"}}}
	llm := okLLM()
	o := newOrch(llm, &fakeRAG{resp: &ragv1.RetrieveResp{}}, st)

	// 检索之后才取历史，避免把本轮刚写入的也算进历史
	_, err := o.Chat(context.Background(), nil,
		&agentv1.ChatReq{Content: "问题", KbIds: []int64{1}}, 1, testIdentity())
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	msgs := llm.chatReq().GetMessages()
	var found bool
	for _, m := range msgs {
		if m.Content == "旧问题" {
			found = true
		}
	}
	if !found {
		t.Error("历史未带入模型上下文")
	}
}

func TestChatHistoryTurnsScaled(t *testing.T) {
	st := &fakeStore{}
	o := newOrch(okLLM(), nil, st)
	// HistoryTurns=5 → 应按 2 倍取（user+assistant 各一条）
	_, _ = o.Chat(context.Background(), nil, &agentv1.ChatReq{Content: "问题"}, 1, testIdentity())
	if st.historyN != 10 {
		t.Errorf("RecentMessages n = %d, 期望 10（5 轮 × 2）", st.historyN)
	}
}

// ---------- ChatStream ----------

func streamOf(frames ...streamFrame) *fakeLLM {
	return &fakeLLM{resp: &llmv1.ChatResp{}, stream: frames}
}

func delta(s string) streamFrame {
	return streamFrame{chunk: &llmv1.ChatChunk{Delta: s}}
}

func TestChatStreamEventOrder(t *testing.T) {
	llm := streamOf(
		delta("你"),
		delta("好"),
		streamFrame{chunk: &llmv1.ChatChunk{Done: true, Usage: &llmv1.Usage{TotalTokens: 42}}},
	)
	st := &fakeStore{}
	o := newOrch(llm, &fakeRAG{resp: &ragv1.RetrieveResp{Hits: []*ragv1.Hit{
		hit("c1", 1, 11, "a.pdf", 0, 0.9, "资料内容"),
	}}}, st)

	var got []*StreamEvent
	err := o.ChatStream(context.Background(),
		&agentv1.Agent{RagEnabled: true, KbIds: []int64{1}},
		&agentv1.ChatReq{Content: "问题"}, 5001, testIdentity(),
		func(e *StreamEvent) bool {
			got = append(got, e)
			return true
		})
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}

	want := []string{EventSession, EventRetrieval, EventCitation, EventDelta, EventDelta, EventDone}
	if len(got) != len(want) {
		t.Fatalf("事件数 = %d, 期望 %d: %v", len(got), len(want), types(got))
	}
	for i, w := range want {
		if got[i].Type != w {
			t.Errorf("第 %d 个事件 = %q, 期望 %q（完整序: %v）", i, got[i].Type, w, types(got))
		}
	}

	// done 必须携带完整结果
	last := got[len(got)-1]
	if last.Message.GetContent() != "你好" {
		t.Errorf("done 未带完整回答: %q", last.Message.GetContent())
	}
	if last.Usage.GetTotalTokens() != 42 {
		t.Errorf("done 未带 usage: %+v", last.Usage)
	}
	if last.SessionID != 5001 {
		t.Errorf("done 的 session_id = %d", last.SessionID)
	}
	// 检索阶段就该把命中数告诉前端（缩短首字等待感）
	if got[1].HitCount != 1 {
		t.Errorf("retrieval 事件 hit_count = %d, 期望 1", got[1].HitCount)
	}
}

func TestChatStreamPersistsUserMessageFirst(t *testing.T) {
	st := &fakeStore{}
	llm := streamOf(streamFrame{chunk: &llmv1.ChatChunk{Delta: "答", Done: true}})
	o := newOrch(llm, nil, st)

	err := o.ChatStream(context.Background(), nil,
		&agentv1.ChatReq{Content: "问题"}, 1, testIdentity(),
		func(*StreamEvent) bool { return true })
	if err != nil {
		t.Fatalf("ChatStream 失败: %v", err)
	}

	msgs := st.appendedMsgs()
	if len(msgs) != 2 {
		t.Fatalf("应落库 2 条（user + assistant），实际 %d", len(msgs))
	}
	if msgs[0].GetRole() != "user" || msgs[0].GetContent() != "问题" {
		t.Errorf("首条应为用户提问: %+v", msgs[0])
	}
	if msgs[1].GetRole() != "assistant" || msgs[1].GetContent() != "答" {
		t.Errorf("次条应为助手回答: %+v", msgs[1])
	}
}

func TestChatStreamStopsWhenClientGone(t *testing.T) {
	// emit 返回 false 表示消费方已断开，必须立即停止读下游
	llm := streamOf(delta("一"), delta("二"), delta("三"))
	st := &fakeStore{}
	o := newOrch(llm, nil, st)

	n := 0
	err := o.ChatStream(context.Background(), nil,
		&agentv1.ChatReq{Content: "问题"}, 1, testIdentity(),
		func(e *StreamEvent) bool {
			n++
			if e.Type == EventSession {
				return true // 放行 session
			}
			return false // 之后全部断开
		})
	if err != nil {
		t.Errorf("客户端断开不应算错误: %v", err)
	}
	if n != 2 {
		t.Errorf("断开后不应继续 emit，实际 emit %d 次", n)
	}
	// 断开后不应再落库助手消息
	if len(st.appendedMsgs()) != 1 {
		t.Errorf("断开后不应落库助手消息，实际 %d 条", len(st.appendedMsgs()))
	}
}

func TestChatStreamEmitsErrorOnBrokenStream(t *testing.T) {
	llm := streamOf(delta("半句"), streamFrame{err: errors.New("连接重置")})
	o := newOrch(llm, nil, &fakeStore{})

	var got []*StreamEvent
	err := o.ChatStream(context.Background(), nil,
		&agentv1.ChatReq{Content: "问题"}, 1, testIdentity(),
		func(e *StreamEvent) bool { got = append(got, e); return true })

	// 流断不应向上抛错（连接已建立），而是转成 error 事件
	if err != nil {
		t.Errorf("流中断不应返回错误: %v", err)
	}
	if len(got) == 0 || got[len(got)-1].Type != EventError {
		t.Fatalf("应发出 error 事件，实际 %v", types(got))
	}
	if got[len(got)-1].ErrorCode != "STREAM_BROKEN" {
		t.Errorf("error code = %q", got[len(got)-1].ErrorCode)
	}
}

func TestChatStreamTreatsEOFAsNormal(t *testing.T) {
	// 没有 Done 帧、直接 EOF 也应正常收尾，不能当错误
	llm := streamOf(delta("答"))
	o := newOrch(llm, nil, &fakeStore{})

	var got []*StreamEvent
	if err := o.ChatStream(context.Background(), nil,
		&agentv1.ChatReq{Content: "问题"}, 1, testIdentity(),
		func(e *StreamEvent) bool { got = append(got, e); return true }); err != nil {
		t.Fatalf("EOF 应视为正常收尾: %v", err)
	}
	if got[len(got)-1].Type != EventDone {
		t.Errorf("末事件 = %q, 期望 done", got[len(got)-1].Type)
	}
}

func TestChatStreamReasoningAndToolCalls(t *testing.T) {
	llm := streamOf(
		streamFrame{chunk: &llmv1.ChatChunk{ReasoningDelta: "想一下"}},
		streamFrame{chunk: &llmv1.ChatChunk{Delta: "答", Done: true}},
	)
	o := newOrch(llm, nil, &fakeStore{})

	var got []*StreamEvent
	_ = o.ChatStream(context.Background(), nil,
		&agentv1.ChatReq{Content: "问题"}, 1, testIdentity(),
		func(e *StreamEvent) bool { got = append(got, e); return true })

	var foundReasoning bool
	for _, e := range got {
		if e.Type == EventReasoning && e.Reasoning == "想一下" {
			foundReasoning = true
		}
	}
	if !foundReasoning {
		t.Errorf("思维链增量应单独成事件: %v", types(got))
	}
}

func TestChatStreamLLMCallError(t *testing.T) {
	llm := &fakeLLM{err: errors.New("模型不可用")}
	o := newOrch(llm, nil, &fakeStore{})
	err := o.ChatStream(context.Background(), nil,
		&agentv1.ChatReq{Content: "问题"}, 1, testIdentity(),
		func(*StreamEvent) bool { return true })
	if err == nil {
		t.Fatal("调用模型失败应返回错误")
	}
}

func TestChatStreamRejectsEmptyContent(t *testing.T) {
	o := newOrch(okLLM(), nil, &fakeStore{})
	called := false
	err := o.ChatStream(context.Background(), nil,
		&agentv1.ChatReq{Content: " "}, 1, testIdentity(),
		func(*StreamEvent) bool { called = true; return true })
	if err == nil {
		t.Fatal("空内容应被拒绝")
	}
	if called {
		t.Error("空内容不应触发任何事件")
	}
}

func TestChatStreamAndSyncProduceSameContext(t *testing.T) {
	// 同一 Agent + 同一问题，流式与非流式发给模型的消息必须一致
	agent := &agentv1.Agent{Model: "m", SystemPrompt: "sys", RagEnabled: true, KbIds: []int64{1}}
	req := &agentv1.ChatReq{Content: "问题"}
	ragResp := &ragv1.RetrieveResp{Hits: []*ragv1.Hit{hit("c1", 1, 11, "a.pdf", 0, 0.9, "资料")}}
	hist := []*agentv1.ChatMessage{{Role: "user", Content: "历史"}}

	syncLLM := okLLM()
	o1 := newOrch(syncLLM, &fakeRAG{resp: ragResp}, &fakeStore{recent: hist})
	if _, err := o1.Chat(context.Background(), agent, req, 1, testIdentity()); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	streamLLM := streamOf(streamFrame{chunk: &llmv1.ChatChunk{Done: true}})
	o2 := newOrch(streamLLM, &fakeRAG{resp: ragResp}, &fakeStore{recent: hist})
	if err := o2.ChatStream(context.Background(), agent, req, 1, testIdentity(),
		func(*StreamEvent) bool { return true }); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	a := syncLLM.chatReq().GetMessages()
	b := streamLLM.streamReq().GetMessages()
	if len(a) != len(b) {
		t.Fatalf("消息数不一致: 同步 %d vs 流式 %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Role != b[i].Role || a[i].Content != b[i].Content {
			t.Errorf("第 %d 条不一致:\n 同步: %+v\n 流式: %+v", i, a[i], b[i])
		}
	}
}

func TestChatStreamDegradationFlagged(t *testing.T) {
	llm := streamOf(streamFrame{chunk: &llmv1.ChatChunk{Delta: "答", Done: true}})
	rag := &fakeRAG{err: errors.New("向量库不可用")}
	o := newOrch(llm, rag, &fakeStore{})

	var done *StreamEvent
	_ = o.ChatStream(context.Background(),
		&agentv1.Agent{RagEnabled: true, KbIds: []int64{1}},
		&agentv1.ChatReq{Content: "问题"}, 1, testIdentity(),
		func(e *StreamEvent) bool {
			if e.Type == EventDone {
				done = e
			}
			return true
		})
	if done == nil {
		t.Fatal("未收到 done 事件")
	}
	if !done.Message.GetDegraded() {
		t.Error("RAG 故障应标记降级")
	}
	if done.Degraded.GetReason() == "" {
		t.Error("降级原因不应为空")
	}
}

func types(evs []*StreamEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// ---------- 辅助函数 ----------

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"短", 10, "短"},
		{"短", 1, "短"},         // 不截断
		{"abcdef", 3, "abc…"}, // 按 rune 截断
		{"中文中文", 2, "中文…"},
		{"", 5, ""},
	}
	for _, c := range cases {
		if got := truncate(c.in, c.n); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, 期望 %q", c.in, c.n, got, c.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "b"); got != "b" {
		t.Errorf("got %q", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("空输入应返回空串, got %q", got)
	}
}

func TestToDegradation(t *testing.T) {
	if toDegradation(nil) != nil {
		t.Error("nil 应返回 nil")
	}
	if toDegradation(&agentv1.ChatMessage{}) != nil {
		t.Error("未降级应返回 nil")
	}
	got := toDegradation(&agentv1.ChatMessage{Degraded: true})
	if got.GetDegraded() != true || got.GetReason() != "未说明原因" {
		t.Errorf("缺原因时应补默认值: %+v", got)
	}
	got = toDegradation(&agentv1.ChatMessage{Degraded: true, DegradeReason: "因为"})
	if got.GetReason() != "因为" {
		t.Errorf("reason = %q", got.GetReason())
	}
}

// ---------- 并发 ----------

func TestOrchestratorConcurrentSafe(t *testing.T) {
	llm := okLLM()
	rag := &fakeRAG{resp: &ragv1.RetrieveResp{Hits: []*ragv1.Hit{
		hit("c1", 1, 11, "a.pdf", 0, 0.9, "资料"),
	}}}
	st := &fakeStore{}
	o := newOrch(llm, rag, st)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := o.Chat(context.Background(),
				&agentv1.Agent{RagEnabled: true, KbIds: []int64{1}},
				&agentv1.ChatReq{Content: fmt.Sprintf("q%d", i)},
				int64(i+1), testIdentity())
			if err != nil {
				t.Errorf("并发 Chat 失败: %v", err)
			}
		}(i)
	}
	wg.Wait()
}

// fakeKBMount 知识库绑定解析替身
type fakeKBMount struct {
	ids  []int64
	err  error
	got  int64
	call int
}

func (f *fakeKBMount) ListAgentKB(ctx context.Context, agentID int64) ([]int64, error) {
	f.got = agentID
	f.call++
	if f.err != nil {
		return nil, f.err
	}
	return f.ids, nil
}

func TestResolveKBMountsFillsPlan(t *testing.T) {
	mount := &fakeKBMount{ids: []int64{2012, 2013}}
	o := New(Options{LLM: okLLM(), RAG: &fakeRAG{}, Store: &fakeStore{},
		Agent: testAgentCfg(), RAGCfg: testRAGCfg(), KBMount: mount})

	plan := AgentPlan{UseRAG: true}
	o.resolveKBMounts(context.Background(), 1005, &plan)

	if len(plan.KbIDs) != 2 || plan.KbIDs[0] != 2012 {
		t.Fatalf("绑定应填充到 plan: %v", plan.KbIDs)
	}
	if !plan.UseRAG {
		t.Fatal("有绑定时应强制开启检索")
	}
	if mount.got != 1005 {
		t.Fatalf("应按 agent_id 拉取，实际 %d", mount.got)
	}
}

func TestResolveKBMountsForcesRAGWhenAgentOff(t *testing.T) {
	// ★ 核心场景：Agent 创建时 rag_enabled=false，但之后管理员绑定了 KB。
	//   绑定本身就是要检索的信号，不能因为开关没开就静默不检。
	mount := &fakeKBMount{ids: []int64{2012}}
	o := New(Options{LLM: okLLM(), RAG: &fakeRAG{}, Store: &fakeStore{},
		Agent: testAgentCfg(), RAGCfg: testRAGCfg(), KBMount: mount})

	plan := AgentPlan{UseRAG: false} // Agent 关闭 RAG
	o.resolveKBMounts(context.Background(), 1005, &plan)

	if !plan.UseRAG || len(plan.KbIDs) != 1 {
		t.Fatalf("有绑定时应强制开启检索: UseRAG=%v KbIDs=%v", plan.UseRAG, plan.KbIDs)
	}
}

func TestResolveKBMountsNoBindingsKeepsAgentSwitch(t *testing.T) {
	mount := &fakeKBMount{ids: nil}
	o := New(Options{LLM: okLLM(), RAG: &fakeRAG{}, Store: &fakeStore{},
		Agent: testAgentCfg(), RAGCfg: testRAGCfg(), KBMount: mount})

	plan := AgentPlan{UseRAG: false} // Agent 明确关闭且无绑定：保持关闭
	o.resolveKBMounts(context.Background(), 1005, &plan)

	if plan.UseRAG || len(plan.KbIDs) != 0 {
		t.Fatalf("无绑定时应尊重 Agent 开关: UseRAG=%v", plan.UseRAG)
	}
}

func TestResolveKBMountsSkipsWhenSpecified(t *testing.T) {
	mount := &fakeKBMount{ids: []int64{999}}
	o := New(Options{LLM: okLLM(), RAG: &fakeRAG{}, Store: &fakeStore{},
		Agent: testAgentCfg(), RAGCfg: testRAGCfg(), KBMount: mount})

	plan := AgentPlan{UseRAG: true, KbIDs: []int64{7}}
	o.resolveKBMounts(context.Background(), 1005, &plan)

	if mount.call != 0 {
		t.Fatalf("显式指定 kb_ids 时不应再去拉绑定，实际调用 %d 次", mount.call)
	}
	if plan.KbIDs[0] != 7 {
		t.Fatal("显式 kb_ids 不应被覆盖")
	}
}

func TestResolveKBMountsErrorSilentlySkips(t *testing.T) {
	mount := &fakeKBMount{err: errors.New("rag down")}
	o := New(Options{LLM: okLLM(), RAG: &fakeRAG{}, Store: &fakeStore{},
		Agent: testAgentCfg(), RAGCfg: testRAGCfg(), KBMount: mount})

	plan := AgentPlan{UseRAG: true}
	o.resolveKBMounts(context.Background(), 1005, &plan)

	if len(plan.KbIDs) != 0 {
		t.Fatalf("拉取失败应静默跳过，实际 %v", plan.KbIDs)
	}
}

func TestResolveKBMountsNoBindingsLeavesPlan(t *testing.T) {
	mount := &fakeKBMount{ids: nil}
	o := New(Options{LLM: okLLM(), RAG: &fakeRAG{}, Store: &fakeStore{},
		Agent: testAgentCfg(), RAGCfg: testRAGCfg(), KBMount: mount})

	plan := AgentPlan{UseRAG: true}
	o.resolveKBMounts(context.Background(), 1005, &plan)

	if len(plan.KbIDs) != 0 {
		t.Fatalf("无绑定时不应改动 plan: %v", plan.KbIDs)
	}
}
