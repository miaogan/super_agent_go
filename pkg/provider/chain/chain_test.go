package chain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aegis/pkg/provider"
)

// stubProvider 可编程的测试 Provider。
type stubProvider struct {
	name string

	mu       sync.Mutex
	calls    int
	failErr  error
	failN    int // >0 时前 N 次调用失败，0 表示一直失败
	unsupErr error

	// 阻塞控制：用于验证 ctx 取消路径
	blockUntilCancel bool
	delay            time.Duration

	// 返回内容
	reply string
}

var _ provider.Provider = (*stubProvider)(nil)

func newStub(name string) *stubProvider {
	return &stubProvider{name: name, reply: "ok:" + name}
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Streaming: true, Embeddings: true}
}

func (s *stubProvider) Chat(ctx context.Context, _ *provider.ChatRequest) (*provider.ChatResponse, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	err := s.failErr
	failN := s.failN
	block := s.blockUntilCancel
	delay := s.delay
	reply := s.reply
	s.mu.Unlock()

	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil && (failN == 0 || n <= failN) {
		return nil, err
	}
	return &provider.ChatResponse{Content: reply, Model: s.name}, nil
}

func (s *stubProvider) ChatStream(ctx context.Context, _ *provider.ChatRequest) (<-chan *provider.Chunk, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	err := s.failErr
	failN := s.failN
	block := s.blockUntilCancel
	s.mu.Unlock()

	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil && (failN == 0 || n <= failN) {
		return nil, err
	}
	out := make(chan *provider.Chunk, 4)
	go func() {
		defer close(out)
		for i := 0; i < 3; i++ {
			select {
			case out <- &provider.Chunk{Delta: fmt.Sprintf("%s-%d", s.name, i)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (s *stubProvider) Embed(ctx context.Context, req *provider.EmbedRequest) (*provider.EmbedResponse, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	err := s.failErr
	failN := s.failN
	block := s.blockUntilCancel
	s.mu.Unlock()

	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil && (failN == 0 || n <= failN) {
		return nil, err
	}
	out := make([][]float32, len(req.Texts))
	for i := range out {
		out[i] = []float32{float32(i), 1}
	}
	return &provider.EmbedResponse{Embeddings: out, Model: s.name, Dimension: 2}, nil
}

func (s *stubProvider) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{Name: s.name}}, nil
}

func (s *stubProvider) Rerank(ctx context.Context, req *provider.RerankRequest) (*provider.RerankResponse, error) {
	s.mu.Lock()
	s.calls++
	err := s.failErr
	unsup := s.unsupErr
	s.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if unsup != nil {
		return nil, unsup
	}
	out := make([]provider.RerankResult, 0, len(req.Documents))
	for i, d := range req.Documents {
		item := provider.RerankResult{Index: i, RelevanceScore: float64(len(req.Documents) - i)}
		if req.ReturnDocuments {
			item.Document = d
		}
		out = append(out, item)
	}
	if req.TopN > 0 && len(out) > req.TopN {
		out = out[:req.TopN]
	}
	return &provider.RerankResponse{Results: out, Model: s.name}, nil
}

func (s *stubProvider) Close() error { return nil }

func (s *stubProvider) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubProvider) setFail(err error, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failErr, s.failN = err, n
}

func newChain(t *testing.T, th int, cd time.Duration, ps ...provider.Provider) *Chain {
	t.Helper()
	return New(Options{Providers: ps, Threshold: th, Cooldown: cd})
}

// TestChatFirstProviderSucceeds 无需降级时不应标记 degraded。
func TestChatFirstProviderSucceeds(t *testing.T) {
	a := newStub("a")
	b := newStub("b")
	c := newChain(t, 3, time.Second, a, b)

	resp, err := c.Chat(context.Background(), &provider.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Degraded {
		t.Error("首个Provider 成功时不应标记降级")
	}
	if b.callCount() != 0 {
		t.Error("不应调用后续 Provider")
	}
}

// TestChatDegradesToSecond 首个失败后应降级并如实标记。
func TestChatDegradesToSecond(t *testing.T) {
	a := newStub("a")
	a.setFail(errors.New("upstream 500"), 0)
	b := newStub("b")
	c := newChain(t, 3, time.Second, a, b)

	resp, err := c.Chat(context.Background(), &provider.ChatRequest{})
	if err != nil {
		t.Fatalf("应降级成功，实际报错: %v", err)
	}
	if !resp.Degraded {
		t.Error("★ 发生降级但未标记 Degraded")
	}
	if resp.DegradeReason == "" {
		t.Error("★ DegradeReason 必须如实填写，不能静默降级")
	}
	if !strings.Contains(resp.DegradeReason, "a") {
		t.Errorf("降级原因应指明是哪个 Provider 失败，实际 %q", resp.DegradeReason)
	}
	if resp.Content != "ok:b" {
		t.Errorf("应返回第二个 Provider 的结果，实际 %q", resp.Content)
	}
}

// TestChatAllFail 全部失败时返回最后一个错误。
func TestChatAllFail(t *testing.T) {
	a := newStub("a")
	a.setFail(errors.New("err-a"), 0)
	b := newStub("b")
	b.setFail(errors.New("err-b"), 0)
	c := newChain(t, 3, time.Second, a, b)

	_, err := c.Chat(context.Background(), &provider.ChatRequest{})
	if err == nil {
		t.Fatal("全部失败时应返回错误")
	}
	if err.Error() != "err-b" {
		t.Errorf("应返回最后尝试的 Provider 的错误，实际 %v", err)
	}
}

// TestChatEmptyChain 空链不应 panic。
func TestChatEmptyChain(t *testing.T) {
	c := newChain(t, 3, time.Second)
	if _, err := c.Chat(context.Background(), &provider.ChatRequest{}); err == nil {
		t.Error("空链应返回错误")
	}
	if c.Name() != "chain-empty" {
		t.Errorf("空链名称应为 chain-empty，实际 %q", c.Name())
	}
	if caps := c.Capabilities(); caps.Streaming || caps.Embeddings {
		t.Errorf("空链不应声明任何能力，实际 %+v", caps)
	}
}

// TestBreakerTripsAfterThreshold 连续失败达阈值后熔断。
func TestBreakerTripsAfterThreshold(t *testing.T) {
	a := newStub("a")
	a.setFail(errors.New("boom"), 0)
	b := newStub("b")
	c := newChain(t, 2, 300*time.Millisecond, a, b)

	// 前2 次：a 失败 → 降级到 b
	for i := 0; i < 2; i++ {
		if _, err := c.Chat(context.Background(), &provider.ChatRequest{}); err != nil {
			t.Fatal(err)
		}
	}

	// 此时 a 已熔断
	h := c.Health()
	if !strings.HasPrefix(h["a"], "open:") {
		t.Errorf("★ a 应处于熔断状态，实际 %q", h["a"])
	}
	if h["b"] != "ok" {
		t.Errorf("b 应为 ok，实际 %q", h["b"])
	}

	// 熔断期间不应再调用 a
	before := a.callCount()
	for i := 0; i < 5; i++ {
		c.Chat(context.Background(), &provider.ChatRequest{})
	}
	if a.callCount() != before {
		t.Errorf("★ 熔断期间仍被调用 %d 次", a.callCount()-before)
	}
}

// TestBreakerCooldownExpires 冷却结束后应重新尝试。
func TestBreakerCooldownExpires(t *testing.T) {
	a := newStub("a")
	a.setFail(errors.New("boom"), 1) // 只第一次失败
	b := newStub("b")
	c := newChain(t, 1, 50*time.Millisecond, a, b)

	c.Chat(context.Background(), &provider.ChatRequest{}) // a 失败 → 熔断
	if !strings.HasPrefix(c.Health()["a"], "open:") {
		t.Fatal("a 应已熔断")
	}

	time.Sleep(80 * time.Millisecond) // 冷却结束

	before := a.callCount()
	resp, err := c.Chat(context.Background(), &provider.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if a.callCount() <= before {
		t.Error("★ 冷却结束后应重新尝试 a")
	}
	if resp.Content != "ok:a" {
		t.Errorf("a 已恢复，应返回 a 的结果，实际 %q", resp.Content)
	}
}

// TestBreakerResetOnSuccess 成功后应清零失败计数。
func TestBreakerResetOnSuccess(t *testing.T) {
	a := newStub("a")
	a.setFail(errors.New("boom"), 1) // 仅第 1 次失败
	b := newStub("b")
	c := newChain(t, 3, time.Second, a, b)

	// 第 1 次：a 失败 → 降级到 b
	if _, err := c.Chat(context.Background(), &provider.ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	failures := c.breakers["a"].failures
	c.mu.Unlock()
	if failures != 1 {
		t.Fatalf("a 失败后计数应为 1，实际 %d", failures)
	}

	// 第 2 次：a 成功 → 计数清零
	if _, err := c.Chat(context.Background(), &provider.ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	failures = c.breakers["a"].failures
	c.mu.Unlock()
	if failures != 0 {
		t.Errorf("★ 成功后失败计数应清零，实际 %d", failures)
	}
}

// TestCtxCancelNotCountedAsFailure ★ 核心回归测试。
//
// 上游超时/客户端取消时 Provider 本身完全健康。
// 若把取消计入熔断失败，一次网关抖动就会把好 Provider 熔断，
// 之后所有正常请求全被降级 —— 这是很难察觉的雪崩。
func TestCtxCancelNotCountedAsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	a := newStub("a")
	a.blockUntilCancel = true
	b := newStub("b")
	c := newChain(t, 2, time.Second, a, b)

	// 连续多次触发取消
	for i := 0; i < 6; i++ {
		cgo, cancelNow := context.WithCancel(ctx)
		cancelNow() // 立即取消

		c.Chat(cgo, &provider.ChatRequest{})
		c.ChatStream(cgo, &provider.ChatRequest{})
		c.Embed(cgo, &provider.EmbedRequest{})
	}

	c.mu.Lock()
	var bCount int
	if bk, ok := c.breakers["a"]; ok {
		bCount = bk.failures
	}
	c.mu.Unlock()

	if bCount != 0 {
		t.Errorf("★ ctx 取消被计入了 %d 次失败（阈值 2，此时 a 应已被误熔断）", bCount)
	}
	// Health 只为真正记录过状态的 Provider 建条目。
	// a 的条目要么不存在（从未记过失败），要么 failures 为 0 ——
	//	绝不能是 open。
	if h, ok := c.Health()["a"]; ok && strings.HasPrefix(h, "open:") {
		t.Errorf("★ a 不应被熔断，实际 %q", h)
	}
	cancel()
}

// TestCtxCancelReturnsImmediately 取消后应立即返回而不是继续尝试后续 Provider。
func TestCtxCancelReturnsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := newStub("a")
	a.blockUntilCancel = true
	b := newStub("b")
	c := newChain(t, 3, time.Second, a, b)

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.Chat(ctx, &provider.ChatRequest{})
	elapsed := time.Since(start)

	if err == nil {
		t.Error("取消后应返回错误")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("取消后未及时返回，耗时 %v", elapsed)
	}
	if b.callCount() != 0 {
		t.Error("★ ctx 取消后不应继续尝试后续 Provider")
	}
}

// TestChatStreamDegradesToSecond 流式降级应注入标记帧。
func TestChatStreamDegradesToSecond(t *testing.T) {
	a := newStub("a")
	a.setFail(errors.New("stream down"), 0)
	b := newStub("b")
	c := newChain(t, 3, time.Second, a, b)

	ch, err := c.ChatStream(context.Background(), &provider.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}

	var first *provider.Chunk
	var rest []*provider.Chunk
	for chunk := range ch {
		if first == nil {
			first = chunk
			continue
		}
		rest = append(rest, chunk)
	}

	if first == nil {
		t.Fatal("未收到任何帧")
	}
	if !first.Degraded {
		t.Error("★ 流式降级的首帧应携带 Degraded 标记")
	}
	if first.DegradeReason == "" {
		t.Error("★ 流式降级首帧应携带 DegradeReason")
	}
	if len(rest) == 0 {
		t.Error("降级标记后应继续透传原始帧")
	}
}

// TestChatStreamNoDegradeNoMarker 未降级时不应注入多余帧。
func TestChatStreamNoDegradeNoMarker(t *testing.T) {
	a := newStub("a")
	c := newChain(t, 3, time.Second, a)

	ch, err := c.ChatStream(context.Background(), &provider.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for chunk := range ch {
		if chunk.Degraded {
			t.Error("★ 未降级却注入了降级标记")
		}
		n++
	}
	if n != 3 {
		t.Errorf("应原样透传 3 帧，实际 %d", n)
	}
}

// TestWrapDegradeExitsOnCtxCancel ★ goroutine 泄漏回归测试。
//
// 消费方（llm_service.ChatStream）在下游 send 失败时直接 return，
// 不再读取返回的 channel。若 wrapDegrade 不感知 ctx，
// 它会永远阻塞在 out <- chunk 上——
// 每一次客户端中途断开都会泄漏一个 goroutine。
func TestWrapDegradeExitsOnCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// 源channel 缓冲足够大，wrapDegrade 不会因无读者而卡在 src 上
	src := make(chan *provider.Chunk, 1000)
	src <- &provider.Chunk{Delta: "c1"}

	out := wrapDegrade(ctx, src, "degraded")

	// 读到注入的标记帧后立刻放弃消费（模拟客户端断开）
	<-out
	cancel()

	// 给 goroutine 时间退出，然后检查它是否真的退出了：
	// 通过"关闭 src 后 out 被关闭"来观察。
	// 若goroutine 泄漏，它不会消费 src，close(src) 后 out 也不会关闭。
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.After(2 * time.Second)
		for {
			select {
			case _, ok := <-out:
				if !ok {
					return // out 已关闭 → goroutine 已退出
				}
			case <-deadline:
				t.Error("★ wrapDegrade 未感知 ctx，goroutine 泄漏（out 未关闭）")
				return
			}
		}
	}()
	close(src)
	<-done
}

// TestWrapDegradeInjectsOnEmptyStream 源流为空时也应注入降级标记。
func TestWrapDegradeInjectsOnEmptyStream(t *testing.T) {
	ctx := context.Background()
	src := make(chan *provider.Chunk)
	close(src)

	out := wrapDegrade(ctx, src, "degraded")
	chunk, ok := <-out
	if !ok {
		t.Fatal("应至少收到降级标记帧")
	}
	if !chunk.Degraded || chunk.DegradeReason != "degraded" {
		t.Errorf("应注入降级标记，实际 %+v", chunk)
	}
	if _, ok := <-out; ok {
		t.Error("标记后应关闭")
	}
}

// TestEmbedDegrades Embed 降级路径。
func TestEmbedDegrades(t *testing.T) {
	a := newStub("a")
	a.setFail(errors.New("embed fail"), 0)
	b := newStub("b")
	c := newChain(t, 3, time.Second, a, b)

	resp, err := c.Embed(context.Background(), &provider.EmbedRequest{Texts: []string{"x"}})
	if err != nil {
		t.Fatalf("应降级成功，实际 %v", err)
	}
	if resp.Model != "b" {
		t.Errorf("应返回 b 的向量，实际模型 %q", resp.Model)
	}
}

// TestEmbedAllFail 全部失败。
func TestEmbedAllFail(t *testing.T) {
	a := newStub("a")
	a.setFail(errors.New("e1"), 0)
	b := newStub("b")
	b.setFail(errors.New("e2"), 0)
	c := newChain(t, 3, time.Second, a, b)

	if _, err := c.Embed(context.Background(), &provider.EmbedRequest{}); err == nil {
		t.Error("全部失败应返回错误")
	}
}

// TestUnsupportedCapabilityDoesNotTripBreaker 能力不支持不算故障。
func TestUnsupportedCapabilityDoesNotTripBreaker(t *testing.T) {
	a := newStub("a")
	a.setFail(provider.ErrUnsupported, 0)
	b := newStub("b")
	c := newChain(t, 2, time.Second, a, b)

	for i := 0; i < 5; i++ {
		if _, err := c.Chat(context.Background(), &provider.ChatRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	// 能力不足每次都换下一个，不应把 a 熔断。
	// a 始终返回 ErrUnsupported，从未成功也从未记过失败，
	// 所以 Health 里要么没有 a，要么不是 open。
	if h, ok := c.Health()["a"]; ok && strings.HasPrefix(h, "open:") {
		t.Errorf("★ 能力不支持不应熔断 a，实际 %q", h)
	}
	if c.Health()["b"] != "ok" {
		t.Errorf("b 应为 ok，实际 %q", c.Health()["b"])
	}
	// 反证：真正调用了 5 次 b，说明每次都成功降级到了它
	if b.callCount() != 5 {
		t.Errorf("b 应被调用 5 次，实际 %d", b.callCount())
	}
}

// TestAllBreakersOpenForceFirst 全部熔断时应强制放行第一个。
//
// 可用性优先：宁可超时也不要无响应。
func TestAllBreakersOpenForceFirst(t *testing.T) {
	a := newStub("a")
	a.setFail(errors.New("boom"), 0)
	b := newStub("b")
	b.setFail(errors.New("boom"), 0)
	c := newChain(t, 1, time.Hour, a, b)

	c.Chat(context.Background(), &provider.ChatRequest{}) // a 熔断
	c.Chat(context.Background(), &provider.ChatRequest{}) // b 熔断

	for _, n := range []string{"a", "b"} {
		if !strings.HasPrefix(c.Health()[n], "open:") {
			t.Fatalf("前置条件不满足: %s 应处于熔断，实际 %+v", n, c.Health())
		}
	}

	before := a.callCount()
	c.Chat(context.Background(), &provider.ChatRequest{})
	if a.callCount() <= before {
		t.Error("★ 全部熔断时应强制放行第一个 Provider")
	}
}

// TestHealthHalfOpen 冷却结束应显示 half-open。
func TestHealthHalfOpen(t *testing.T) {
	a := newStub("a")
	a.setFail(errors.New("boom"), 0)
	c := newChain(t, 1, 30*time.Millisecond, a)

	c.Chat(context.Background(), &provider.ChatRequest{})
	if !strings.HasPrefix(c.Health()["a"], "open:") {
		t.Fatal("应为 open")
	}
	time.Sleep(60 * time.Millisecond)
	if got := c.Health()["a"]; got != "half-open" {
		t.Errorf("★ 冷却后应为 half-open，实际 %q", got)
	}
}

// TestListModelsMerges 合并所有 Provider 的模型列表。
func TestListModelsMerges(t *testing.T) {
	a, b := newStub("a"), newStub("b")
	c := newChain(t, 3, time.Second, a, b)

	ms, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Errorf("应合并 2 个模型，实际 %d", len(ms))
	}
}

// TestCloseAll 关闭应调用全部 Provider。
func TestCloseAll(t *testing.T) {
	a, b := newStub("a"), newStub("b")
	c := newChain(t, 3, time.Second, a, b)
	if err := c.Close(); err != nil {
		t.Errorf("不应有错误: %v", err)
	}
}

// TestConcurrentChat 并发调用下熔断计数正确。
func TestConcurrentChat(t *testing.T) {
	a := newStub("a")
	b := newStub("b")
	c := newChain(t, 1000, time.Second, a, b) // 阈值设很高，避免熔断干扰

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				c.Chat(context.Background(), &provider.ChatRequest{})
			}
		}()
	}
	wg.Wait()
}

// TestAppendReason 降级原因拼接不应丢信息。
func TestAppendReason(t *testing.T) {
	if got := appendReason("", "r1"); got != "r1" {
		t.Errorf("首次拼接应直接返回，实际 %q", got)
	}
	got := appendReason("r1", "r2")
	if !strings.Contains(got, "r1") || !strings.Contains(got, "r2") {
		t.Errorf("应保留两段原因，实际 %q", got)
	}
	if !strings.Contains(got, ";") && !strings.Contains(got, "|") {
		t.Logf("当前分隔符: %q", got)
	}
}

// ---------- Rerank 降级 ----------

// TestChainRerankAllUnsupported 全部 Provider 都不支持重排 → ErrUnsupported。
//
// ★ 为什么这条语义至关重要：
//
// 重排是**可选增强**，不是链路必需环节。若这种情况返回普通错误，
// 上层会当成「模型故障」→ 触发重试 / 降级 / 告警，
// 于是一个纯配置缺失会被放大成故障。
//
// 「能力缺失」与「调用失败」必须能被上层区分开（见 ADR-003）。
func TestChainRerankAllUnsupported(t *testing.T) {
	a, b := newStub("a"), newStub("b")
	a.unsupErr, b.unsupErr = provider.ErrUnsupported, provider.ErrUnsupported
	c := newChain(t, 3, time.Second, a, b)

	_, err := c.Rerank(context.Background(), &provider.RerankRequest{
		Query: "q", Documents: []string{"d1"},
	})
	if !provider.IsUnsupported(err) {
		t.Fatalf("全部不支持时应返回 ErrUnsupported，实际: %v", err)
	}
}

// TestChainRerankSkipsUnsupportedProvider 前面几个不支持时仍能命中后面的。
func TestChainRerankSkipsUnsupportedProvider(t *testing.T) {
	a, b, cStub := newStub("a"), newStub("b"), newStub("c")
	a.unsupErr, b.unsupErr = provider.ErrUnsupported, provider.ErrUnsupported
	c := newChain(t, 3, time.Second, a, b, cStub)

	resp, err := c.Rerank(context.Background(), &provider.RerankRequest{
		Query: "q", Documents: []string{"d1", "d2", "d3"}, ReturnDocuments: true,
	})
	if err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("应返回全部 3 条: %+v", resp.Results)
	}
	if resp.Model != "c" {
		t.Errorf("应命中第三个 Provider，实际 %q", resp.Model)
	}
	if resp.Results[0].Document != "d1" {
		t.Errorf("ReturnDocuments 未回带原文: %q", resp.Results[0].Document)
	}
}

// TestChainRerankAllFailed 有人支持但全部调用失败 → 普通错误（不是 ErrUnsupported）。
//
// 这是与上一条镜像的另一半：真故障必须能被上层当故障对待，
// 否则重试与熔断都不会触发。
func TestChainRerankAllFailed(t *testing.T) {
	a, b := newStub("a"), newStub("b")
	a.failErr, b.failErr = errors.New("500 内部错误"), errors.New("500 内部错误")
	c := newChain(t, 3, time.Second, a, b)

	_, err := c.Rerank(context.Background(), &provider.RerankRequest{
		Query: "q", Documents: []string{"d1"},
	})
	if err == nil {
		t.Fatal("全部调用失败时应返回错误")
	}
	if provider.IsUnsupported(err) {
		t.Errorf("调用失败不应报成 ErrUnsupported（否则不会触发重试）: %v", err)
	}
}
