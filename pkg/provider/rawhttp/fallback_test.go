package rawhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/provider"
)

// chatServer 造一个 OpenAI 兼容端点：按 statusFn 决定响应，并累计请求数。
func chatServer(t *testing.T, statusFn func(call int) int, content string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if st := statusFn(n); st != http.StatusOK {
			w.WriteHeader(st)
			fmt.Fprintf(w, `{"error":"boom %d"}`, n)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`, content)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// twoModels 主模型 a（priority 1）+ 备模型 b（priority 2）。
func twoModels(aURL, bURL string) conf.LLMConf {
	return conf.LLMConf{
		Provider:     "rawhttp",
		DefaultModel: "a",
		Timeout:      2 * time.Second,
		Models: []conf.ModelConf{
			{Name: "b", BaseURL: bURL, RealModel: "b-real", Priority: 2, Status: "enabled"},
			{Name: "a", BaseURL: aURL, RealModel: "a-real", Priority: 1, Status: "enabled"},
		},
	}
}

func chatReq() *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    "a",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}
}

// TestChatFallsOverOn5xx 主模型 5xx → 自动改用备选，并如实标记降级。
func TestChatFallsOverOn5xx(t *testing.T) {
	a, aCalls := chatServer(t, func(int) int { return http.StatusInternalServerError }, "")
	b, bCalls := chatServer(t, func(int) int { return http.StatusOK }, "from-b")

	p := New(Options{Conf: twoModels(a.URL, b.URL)})
	defer p.Close()

	out, err := p.Chat(context.Background(), chatReq())
	if err != nil {
		t.Fatalf("应降级成功, 实际 %v", err)
	}
	if out.Content != "from-b" || out.Model != "b" {
		t.Errorf("结果应来自备模型 b: model=%q content=%q", out.Model, out.Content)
	}
	if !out.Degraded || out.DegradeReason == "" {
		t.Error("降级必须如实标记，不能静默")
	}
	if !strings.Contains(out.DegradeReason, "a") {
		t.Errorf("降级原因应指出原模型: %q", out.DegradeReason)
	}
	if aCalls.Load() != 1 || bCalls.Load() != 1 {
		t.Errorf("应各调一次, 实际 a=%d b=%d", aCalls.Load(), bCalls.Load())
	}
}

// TestChatNoFallbackOn4xx 4xx（鉴权/参数）换模型必然同样失败，不该白等一次超时。
func TestChatNoFallbackOn4xx(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound} {
		a, _ := chatServer(t, func(int) int { return code }, "")
		b, bCalls := chatServer(t, func(int) int { return http.StatusOK }, "should-not-be-used")

		p := New(Options{Conf: twoModels(a.URL, b.URL)})
		_, err := p.Chat(context.Background(), chatReq())
		if err == nil {
			t.Errorf("HTTP %d 应返回错误", code)
		}
		if got := bCalls.Load(); got != 0 {
			t.Errorf("HTTP %d 不应降级重试, 备模型被调了 %d 次", code, got)
		}
		p.Close()
		a.Close()
		b.Close()
	}
}

// TestChatNoFallbackWhenDisabled 显式关闭降级时，只调主模型一次。
func TestChatNoFallbackWhenDisabled(t *testing.T) {
	a, _ := chatServer(t, func(int) int { return 503 }, "")
	b, bCalls := chatServer(t, func(int) int { return http.StatusOK }, "x")

	cfg := twoModels(a.URL, b.URL)
	cfg.FallbackDisabled = true
	p := New(Options{Conf: cfg})
	defer p.Close()

	if _, err := p.Chat(context.Background(), chatReq()); err == nil {
		t.Fatal("关闭降级后应直接把主模型的失败抛出")
	}
	if got := bCalls.Load(); got != 0 {
		t.Errorf("已关闭降级, 备模型却被调用 %d 次", got)
	}
}

// TestChatNoFallbackWhenCtxExpired 调用方期限已过就不再重试 ——
// 否则总耗时无界，且注定失败的重试只会拖慢返回。
func TestChatNoFallbackWhenCtxExpired(t *testing.T) {
	a, _ := chatServer(t, func(int) int {
		time.Sleep(300 * time.Millisecond)
		return http.StatusServiceUnavailable
	}, "")
	b, bCalls := chatServer(t, func(int) int { return http.StatusOK }, "x")

	cfg := twoModels(a.URL, b.URL)
	cfg.Timeout = 2 * time.Second
	p := New(Options{Conf: cfg})
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if _, err := p.Chat(ctx, chatReq()); err == nil {
		t.Fatal("超时应报错")
	}
	if got := bCalls.Load(); got != 0 {
		t.Errorf("ctx 已超时时不应重试, 备模型被调 %d 次", got)
	}
}

// TestChatEmptyChoicesDoesNotPanic 回归：旧实现在长度检查之前就取
// Choices[0]，厂商返回空 choices（内容审核拦截等）时整个进程 panic。
func TestChatEmptyChoicesDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
	}))
	defer srv.Close()

	cfg := twoModels(srv.URL, srv.URL)
	cfg.FallbackDisabled = true
	p := New(Options{Conf: cfg})
	defer p.Close()

	out, err := p.Chat(context.Background(), chatReq())
	if err != nil {
		t.Fatalf("空 choices 不算调用失败: %v", err)
	}
	if out.Content != "" {
		t.Errorf("内容应为空: %q", out.Content)
	}
	if out.Usage.TotalTokens != 1 {
		t.Errorf("usage 仍应被解析: %+v", out.Usage)
	}
}

// TestChatCandidatesExcludesEmbedAndRerank 向量/重排模型不能当对话备选 ——
// 它们走的是另一套接口，拿来做降级必然失败，只会白耗一次超时。
func TestChatCandidatesExcludesEmbedAndRerank(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	cfg := conf.LLMConf{
		Provider:          "rawhttp",
		DefaultModel:      "chat-a",
		DefaultEmbedModel: "embed-1",
		Models: []conf.ModelConf{
			{Name: "chat-a", BaseURL: srv.URL, Priority: 1, Status: "enabled"},
			{Name: "chat-b", BaseURL: srv.URL, Priority: 2, Status: "enabled"},
			{Name: "embed-1", BaseURL: srv.URL, Priority: 3, Status: "enabled",
				EmbedNativePath: "text-embedding-v3/text-embedding-v3"},
			// 默认名之外也可能配 embed 模型：靠特征字段兜住
			{Name: "embed-2", BaseURL: srv.URL, Priority: 4, Status: "enabled",
				EmbedNativePath: "x/y"},
			{Name: "rerank-1", BaseURL: srv.URL, Priority: 5, Status: "enabled",
				RerankPath: "text-rerank/text-rerank"},
			{Name: "chat-off", BaseURL: srv.URL, Priority: 6, Status: "disabled"},
		},
	}
	p := New(Options{Conf: cfg})

	got := p.chatCandidates("chat-a")
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	want := "chat-a chat-b"
	if strings.Join(names, " ") != want {
		t.Errorf("候选应为 [%s], 实际 %v", want, names)
	}
	// 请求点名的模型必须排第一，即使它的 priority 不是最小
	got2 := p.chatCandidates("chat-b")
	if len(got2) == 0 || got2[0].Name != "chat-b" {
		t.Errorf("点名模型应排第一, 实际 %v", got2)
	}
}

// TestChatCandidatesOrderRespectsPriority 候选顺序按 priority 升序。
func TestChatCandidatesOrderRespectsPriority(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	p := New(Options{Conf: conf.LLMConf{
		DefaultModel: "mid",
		Models: []conf.ModelConf{
			{Name: "mid", BaseURL: srv.URL, Priority: 5, Status: "enabled"},
			{Name: "late", BaseURL: srv.URL, Priority: 99, Status: "enabled"},
			{Name: "early", BaseURL: srv.URL, Priority: 1, Status: "enabled"},
		},
	}})
	var names []string
	for _, m := range p.chatCandidates("mid") {
		names = append(names, m.Name)
	}
	if strings.Join(names, " ") != "mid early late" {
		t.Errorf("顺序应按 priority 升序且点名优先, 实际 %v", names)
	}
}

// TestChatStreamFallsOverBeforeFirstByte 流式的降级窗口在响应头阶段。
func TestChatStreamFallsOverBeforeFirstByte(t *testing.T) {
	a, _ := chatServer(t, func(int) int { return http.StatusTooManyRequests }, "")

	// 备模型必须真的吐 SSE：ChatStream 用 consumeSSE 解析，
	// 给它非流式 JSON 会得到零个 chunk，测不到内容（夹具会假失败）。
	var bCalls atomic.Int64
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"stream-from-b"}}]}`)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(b.Close)

	p := New(Options{Conf: twoModels(a.URL, b.URL)})
	defer p.Close()

	ch, err := p.ChatStream(context.Background(), chatReq())
	if err != nil {
		t.Fatalf("429 应触发降级, 实际 %v", err)
	}
	var got string
	for c := range ch {
		if c != nil && c.Delta != "" {
			got += c.Delta
		}
	}
	if got := bCalls.Load(); got != 1 {
		t.Errorf("备模型应被调用一次, 实际 %d", got)
	}
	if !strings.Contains(got, "stream-from-b") {
		t.Errorf("流内容应来自备模型, 实际 %q", got)
	}
}

// TestChatStreamNoFallbackAfterStreamStarted ★ 核心约束：
// 一旦开始收到 chunk，就不能再改用另一个模型 —— 那会把两个模型的输出
// 拼成一条通顺但错误的答案，比直接失败更糟。
//
// 这里主模型先回 200 再在流中途断开；期望是"只调主模型一次、不碰备模型"。
func TestChatStreamNoFallbackAfterStreamStarted(t *testing.T) {
	aCalls := &atomic.Int64{}
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 先吐一个合法 chunk，再在流中途断掉（不给 [DONE]）
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"half"}}]}`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer a.Close()

	b, bCalls := chatServer(t, func(int) int { return http.StatusOK }, "should-not-run")

	p := New(Options{Conf: twoModels(a.URL, b.URL)})
	defer p.Close()

	ch, err := p.ChatStream(context.Background(), chatReq())
	if err != nil {
		t.Fatalf("200 后不应报错: %v", err)
	}
	for range ch { //  drain 到通道关闭
	}

	if got := aCalls.Load(); got != 1 {
		t.Errorf("主模型应只被调用一次, 实际 %d", got)
	}
	if got := bCalls.Load(); got != 0 {
		t.Errorf("已经开始收流后不得降级到其他模型, 备模型被调 %d 次", got)
	}
}

// TestStatusErrorMessageUnchanged 确保新增类型没有改变对外错误文本
// （既有调用方与断言按文本匹配）。
func TestStatusErrorMessageUnchanged(t *testing.T) {
	e := &StatusError{Path: "/v1/chat/completions", Code: 500, Body: "oops"}
	want := `rawhttp: /v1/chat/completions 状态 500: oops`
	if e.Error() != want {
		t.Errorf("错误文本变了\n want %q\n got  %q", want, e.Error())
	}
}

// TestRetriableClassification 分类表逐项钉住，避免以后"顺手"放宽。
func TestRetriableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"500", &StatusError{Code: 500}, true},
		{"503", &StatusError{Code: 503}, true},
		{"408", &StatusError{Code: 408}, true},
		{"429", &StatusError{Code: 429}, true},
		{"400", &StatusError{Code: 400}, false},
		{"401", &StatusError{Code: 401}, false},
		{"404", &StatusError{Code: 404}, false},
		{"本地解码错误", errors.New("rawhttp: 解析响应失败"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retriableChatErr(context.Background(), c.err); got != c.want {
				t.Errorf("retriableChatErr(%v)=%v want %v", c.err, got, c.want)
			}
		})
	}

	// ctx 已取消时，即使是可重试状态码也不再试
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if retriableChatErr(dead, &StatusError{Code: 503}) {
		t.Error("ctx 已取消时不应再重试")
	}
}
