package sse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/kitex/pkg/remote/trans/nphttp2/metadata"

	agentv1 "github.com/aegis/idl/kitex_gen/agentv1"
	agentservice "github.com/aegis/idl/kitex_gen/agentv1/agentservice"

	"github.com/aegis/gateway/internal/middleware"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
)

// ---- 假流 ----

// fakeStream 逐个产出预置事件
type fakeStream struct {
	mu     sync.Mutex
	events []*agentv1.ChatEvent
	i      int
	err    error
	// onRecv 记录 Recv 被调用的次数，验证客户端断开后是否停止读取
	onRecv func()
	closed bool
}

func (f *fakeStream) Recv() (*agentv1.ChatEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onRecv != nil {
		f.onRecv()
	}
	if f.i >= len(f.events) {
		if f.err != nil {
			return nil, f.err
		}
		return nil, io.EOF
	}
	ev := f.events[f.i]
	f.i++
	return ev, nil
}

func (f *fakeStream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// ---- streaming.Stream 接口其余方法（本测试无需真实行为）----

func (f *fakeStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeStream) SetTrailer(metadata.MD)       {}
func (f *fakeStream) Header() (metadata.MD, error) { return nil, nil }
func (f *fakeStream) Trailer() metadata.MD         { return nil }
func (f *fakeStream) Context() context.Context     { return context.Background() }
func (f *fakeStream) RecvMsg(interface{}) error    { return io.EOF }
func (f *fakeStream) SendMsg(interface{}) error    { return nil }

// fakeAgent 返回预置流
type fakeAgent struct {
	stream agentservice.AgentService_ChatStreamClient
	err    error

	// ★ gotReq 由 HTTP handler 协程写、测试主协程读，必须加锁。
	//   不加锁时 -race 会报，且断言结果随机失败。
	mu     sync.Mutex
	gotReq *agentv1.ChatReq
}

func (f *fakeAgent) ChatStream(_ context.Context, req *agentv1.ChatReq) (agentservice.AgentService_ChatStreamClient, error) {
	f.mu.Lock()
	f.gotReq = req
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.stream, nil
}

func (f *fakeAgent) request() *agentv1.ChatReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotReq
}

func newCtx() *app.RequestContext {
	c := app.NewContext(0)
	c.Request.SetMethod("GET")
	c.Request.SetRequestURI("/api/v1/agents/1/chat/stream")
	return c
}

func TestMarshalEventTypes(t *testing.T) {
	cases := []struct {
		name  string
		ev    *agentv1.ChatEvent
		wantK []string
	}{
		{"session", &agentv1.ChatEvent{Type: "session", SessionId: 77}, []string{"type", "session_id"}},
		{"retrieval", &agentv1.ChatEvent{Type: "retrieval", HitCount: 2, RetrieveMs: 5}, []string{"type", "hit_count", "retrieve_ms"}},
		{"delta", &agentv1.ChatEvent{Type: "delta", Delta: "你好"}, []string{"type", "delta"}},
		{"reasoning", &agentv1.ChatEvent{Type: "reasoning", Delta: "想"}, []string{"type", "delta"}},
		{"error", &agentv1.ChatEvent{Type: "error", ErrorCode: "X", ErrorMessage: "y"}, []string{"type", "code", "message"}},
	}
	for _, tc := range cases {
		s, err := marshalEvent(tc.ev)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("%s: JSON 非法: %s", tc.name, s)
		}
		for _, k := range tc.wantK {
			if _, ok := m[k]; !ok {
				t.Fatalf("%s 缺少字段 %q: %s", tc.name, k, s)
			}
		}
	}
}

// ★ 事件类型必须原样透传给前端，
//
//	前端靠它决定渲染逻辑。改成"友好名称"会让前端全线失配。
func TestMarshalEventKeepsRawType(t *testing.T) {
	s, _ := marshalEvent(&agentv1.ChatEvent{Type: "citation"})
	if !strings.Contains(s, `"type":"citation"`) {
		t.Fatalf("type 被改写: %s", s)
	}
}

// ★ done 事件必须同时带回答正文与引用，
//
//	只有正文没有引用等于RAG 白做——用户无法溯源。
func TestMarshalDoneIncludesContentAndCitations(t *testing.T) {
	ev := &agentv1.ChatEvent{
		Type: "done",
		Message: &agentv1.ChatMessage{
			SessionId: 1,
			Content:   "答案[1]",
			Citations: []*agentv1.Citation{{Index: 1, DocId: 9, DocTitle: "d.md", Score: 0.8}},
		},
		Usage: &agentv1.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
	}
	s, _ := marshalEvent(ev)
	for _, want := range []string{`"content":"答案[1]"`, `"citations"`, `"doc_title":"d.md"`, `"total_tokens":30`} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺少 %s: %s", want, s)
		}
	}
}

// 降级信息必须如实透出，不能悄悄隐藏——
// 用户需要知道"这个回答没有基于你的知识库"
func TestMarshalDoneExposesDegradation(t *testing.T) {
	ev := &agentv1.ChatEvent{
		Type: "done",
		Message: &agentv1.ChatMessage{
			Content:       "回答",
			Degraded:      true,
			DegradeReason: "vector_db_unavailable",
		},
		Degradation: &commonv1.Degradation{Degraded: true, Reason: "vector_db_unavailable"},
	}
	s, _ := marshalEvent(ev)
	if !strings.Contains(s, "vector_db_unavailable") {
		t.Fatalf("降级原因未透出: %s", s)
	}
	if !strings.Contains(s, `"degraded":true`) {
		t.Fatalf("degraded 标记未透出: %s", s)
	}
}

// ★ 手工构造 map 的意义：不能出现 pb 内部字段。
//
//	直接 json.Marshal(pb) 会带出 state/sizeCache/unknownFields
//	以及被 base64 编码的 bytes，前端根本用不了。
func TestMarshalNoInternalFields(t *testing.T) {
	s, _ := marshalEvent(&agentv1.ChatEvent{Type: "delta", Delta: "x"})
	for _, bad := range []string{"state", "sizeCache", "unknownFields", "XXX_"} {
		if strings.Contains(s, bad) {
			t.Fatalf("泄漏内部字段 %q: %s", bad, s)
		}
	}
}

func TestMarshalCitationNilSafe(t *testing.T) {
	// nil citation 不应 panic（会打断整个流）
	s, err := marshalEvent(&agentv1.ChatEvent{Type: "citation", Citation: nil})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(s, `"type":"citation"`) {
		t.Fatalf("s = %s", s)
	}
}

func TestMarshalCitationsSkipsNil(t *testing.T) {
	got := marshalCitations([]*agentv1.Citation{nil, {Index: 2}, nil})
	if len(got) != 1 {
		t.Fatalf("期望跳过 nil，len=%d", len(got))
	}
}

func TestIsEOF(t *testing.T) {
	if !isEOF(io.EOF) {
		t.Fatal("io.EOF 应识别为 EOF")
	}
	if !isEOF(errors.New("transport: EOF")) {
		t.Fatal("包装过的 EOF 也应识别")
	}
	if isEOF(errors.New("boom")) {
		t.Fatal("普通错误不应识别为 EOF")
	}
}

// TestForwardSetsStreamHeaders ★ 最容易被漏掉的一组头：
//
//	少任何一个，前端表现为"一直转圈"或"内容一次性蹦出来"。
// ---- 集成测试：真实 HTTP 服务器 ----
//
// SSE 的行为（Flush、chunked 编码、断线检测）只在真实连接上才成立。
// 用 app.NewContext 构造的假连接没有网络栈，
// 官方 sse.Writer 会在写header 时解引用 nil 而 panic，
// 测出来的东西没有意义。

// startSSE 起一个真实 Hertz 服务器，把 agent 事件转成 SSE
func startSSE(t *testing.T, ag AgentClient, opt Options) string {
	t.Helper()

	// 由辅助函数统一注入 client，避免每个用例漏填 Client 字段
	// （漏填时表现为 Forward 内 nil 解引用，报错位置与真实原因无关）
	opt.Client = ag

	addr := freeAddr(t)
	h := middleware.NewEngine(addr, 0, 0, 0)
	h.GET("/stream", func(ctx context.Context, c *app.RequestContext) {
		Forward(c, &agentv1.ChatReq{Content: "hi"}, opt)
	})
	go h.Spin()

	base := "http://" + addr
	// 等端口真正就绪
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { _ = h.Close() })
	return base
}

// freeAddr 向内核申请一个空闲端口并立即释放。
//
// Hertz 不暴露实际监听地址（WithHostPorts("127.0.0.1:0") 也拿不到），
// 所以只能先占一个空闲端口再用。
// 残留的抢占窗口极小，且仅影响测试并行度。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请端口失败: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// readUntil 读到 SSE 结束或超时
func readUntil(t *testing.T, body io.Reader, want string) string {
	t.Helper()
	buf := make([]byte, 4096)
	var acc strings.Builder
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, err := body.Read(buf)
		if n > 0 {
			acc.Write(buf[:n])
			if strings.Contains(acc.String(), want) {
				return acc.String()
			}
		}
		if err != nil {
			break
		}
	}
	return acc.String()
}

// ★ 这组头少任何一个，前端就表现为"一直转圈"或"内容一次性蹦出来"。
func TestForwardSetsStreamHeaders(t *testing.T) {
	stream := &fakeStream{events: []*agentv1.ChatEvent{{Type: "done", Message: &agentv1.ChatMessage{Content: "ok"}}}}
	base := startSSE(t, &fakeAgent{stream: stream}, Options{Timeout: 2 * time.Second, Retry: 3 * time.Second})

	resp, err := http.Get(base + "/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	// Nginx 缓冲会让流式彻底退化成非流式
	if x := resp.Header.Get("X-Accel-Buffering"); x != "no" {
		t.Fatalf("X-Accel-Buffering = %q", x)
	}
	readUntil(t, resp.Body, "event: done")
}

// retry 必须是 SSE 协议的一部分，写 HTTP 头无效
func TestForwardEmitsRetryAsSSEField(t *testing.T) {
	stream := &fakeStream{events: []*agentv1.ChatEvent{{Type: "done"}}}
	base := startSSE(t, &fakeAgent{stream: stream}, Options{Timeout: 2 * time.Second, Retry: 2500 * time.Millisecond})

	resp, err := http.Get(base + "/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if got := readUntil(t, resp.Body, "event: done"); !strings.Contains(got, "retry: 2500") {
		t.Fatalf("未输出 retry 帧:\n%s", got)
	}
}

// 事件顺序必须是 session → retrieval → delta → done，
// 前端按到达顺序渲染，错序会先渲染出正文再补引用
func TestForwardStreamsEventsInOrder(t *testing.T) {
	stream := &fakeStream{events: []*agentv1.ChatEvent{
		{Type: "session", SessionId: 1},
		{Type: "retrieval", HitCount: 1},
		{Type: "delta", Delta: "你"},
		{Type: "delta", Delta: "好"},
		{Type: "done", Message: &agentv1.ChatMessage{Content: "你好"}},
	}}
	base := startSSE(t, &fakeAgent{stream: stream}, Options{Timeout: 2 * time.Second})

	resp, err := http.Get(base + "/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body := readUntil(t, resp.Body, "event: done")

	order := []string{"event: session", "event: retrieval", "event: delta", "event: done"}
	last := -1
	for _, o := range order {
		i := strings.Index(body, o)
		if i < 0 {
			t.Fatalf("缺少事件 %s:\n%s", o, body)
		}
		if i < last {
			t.Fatalf("事件乱序：%s 出现在更早位置\n%s", o, body)
		}
		last = i
	}
	// 递增 id 让浏览器断线重连时能续上
	if !strings.Contains(body, "id: 1") || !strings.Contains(body, "id: 2") {
		t.Fatalf("事件缺少递增 id:\n%s", body)
	}
}

func TestForwardUpstreamErrorBecomesErrorEvent(t *testing.T) {
	base := startSSE(t, &fakeAgent{err: errors.New("connection refused")}, Options{Timeout: 2 * time.Second})

	resp, err := http.Get(base + "/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body := readUntil(t, resp.Body, "event: error")
	if !strings.Contains(body, "event: error") {
		t.Fatalf("下游失败应输出 error 事件:\n%s", body)
	}
	// 响应头已发出，无法再改状态码，只能用事件告知前端
	if !strings.Contains(body, "UPSTREAM_UNAVAILABLE") {
		t.Fatalf("缺少错误码:\n%s", body)
	}
}

func TestForwardPassesRequestThrough(t *testing.T) {
	fa := &fakeAgent{stream: &fakeStream{events: []*agentv1.ChatEvent{{Type: "done"}}}}
	base := startSSE(t, fa, Options{Timeout: 2 * time.Second})

	resp, err := http.Get(base + "/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	readUntil(t, resp.Body, "event: done")

	if got := fa.request(); got.GetContent() != "hi" {
		t.Fatalf("请求未透传: %+v", got)
	}
}

// ★ 客户端断开后必须停止读取下游，
//
//	否则白白消耗模型 token —— 用户已经关页面了，
//	生成还在烧钱。
func TestForwardStopsReadingAfterClientClose(t *testing.T) {
	blocking := &blockingStream{}
	base := startSSE(t, &fakeAgent{stream: blocking}, Options{Timeout: 5 * time.Second})

	resp, err := http.Get(base + "/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	// 读一点就断开，模拟用户关页面
	buf := make([]byte, 256)
	_, _ = resp.Body.Read(buf)
	resp.Body.Close()

	// 给转发协程一点时间感知断开
	time.Sleep(300 * time.Millisecond)
	after := blocking.calls.Load()
	time.Sleep(300 * time.Millisecond)
	grown := blocking.calls.Load() - after

	// 断开后不应持续增长（允许少量在途事件）
	if grown > 10 {
		t.Fatalf("客户端断开后仍在持续读取下游: +%d 次/300ms（总计 %d）", grown, blocking.calls.Load())
	}
}

// ---- 慢速模型流：用于验证断线后停止读取 ----

// atomicCounter 轻量计数器（避免为测试引入额外依赖）
type atomicCounter struct{ n atomic.Int64 }

func (c *atomicCounter) Add(d int64) { c.n.Add(d) }
func (c *atomicCounter) Load() int64 { return c.n.Load() }

// blockingStream 模拟慢速模型：每次 Recv 阻塞 50ms 后返回一个 delta
type blockingStream struct {
	calls atomicCounter
}

func (b *blockingStream) Recv() (*agentv1.ChatEvent, error) {
	b.calls.Add(1)
	time.Sleep(50 * time.Millisecond)
	return &agentv1.ChatEvent{Type: "delta", Delta: "x"}, nil
}

func (b *blockingStream) Close() error { return nil }

func (b *blockingStream) SetHeader(metadata.MD) error  { return nil }
func (b *blockingStream) SendHeader(metadata.MD) error { return nil }
func (b *blockingStream) SetTrailer(metadata.MD)       {}
func (b *blockingStream) Header() (metadata.MD, error) { return nil, nil }
func (b *blockingStream) Trailer() metadata.MD         { return nil }
func (b *blockingStream) Context() context.Context     { return context.Background() }
func (b *blockingStream) RecvMsg(interface{}) error    { return io.EOF }
func (b *blockingStream) SendMsg(interface{}) error    { return nil }
