// Package chain 实现 Provider 降级链。
//
// 场景：配置了多个同厂商模型（或多个厂商），主模型超时/限流时自动降级。
//
// 关键设计：
//   - 降级链在 Provider 层而非 handler 层，
//     这样 agent_service 完全不需要知道"当前用的是第几个模型"
//   - 每次降级都会在 ChatResponse.DegradeReason 中如实记录，
//     绝不静默 —— 上层必须能把"降级了"这件事告诉用户
//   - 熔断：连续失败的模型会被临时跳过，避免每次请求都先撞一次超时
package chain

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/provider"
)

// breaker 单个 Provider 的熔断状态
type breaker struct {
	failures  int
	openUntil time.Time
	lastErr   error
}

// Chain 降级链
type Chain struct {
	providers []provider.Provider
	th        int // 单个 Provider 熔断阈值
	cooldown  time.Duration

	mu       sync.Mutex
	breakers map[string]*breaker

	// closed 缓存"当前可用"的候选，避免每次请求都遍历
	closed bool
}

// Options 构造参数
type Options struct {
	Providers []provider.Provider
	// Threshold 连续失败多少次后熔断，默认 3
	Threshold int
	// Cooldown 熔断冷却时长，默认 30s
	Cooldown time.Duration
}

// New 构造降级链
func New(opt Options) *Chain {
	if opt.Threshold <= 0 {
		opt.Threshold = 3
	}
	if opt.Cooldown <= 0 {
		opt.Cooldown = 30 * time.Second
	}
	return &Chain{
		providers: opt.Providers,
		th:        opt.Threshold,
		cooldown:  opt.Cooldown,
		breakers:  make(map[string]*breaker),
		closed:    true,
	}
}

var _ provider.Provider = (*Chain)(nil)

// errNoProviders 链上没有任何 Provider（通常是配置缺失或写错）
var errNoProviders = errors.New("chain: 未配置任何 Provider")

// Name 返回链上首个 Provider 名（便于日志与健康检查）
func (c *Chain) Name() string {
	if len(c.providers) == 0 {
		return "chain-empty"
	}
	return c.providers[0].Name()
}

// Capabilities 取"当前主 Provider"的能力声明。
//
// 语义：调用方应按最弱的那个（首个可用的）来设计降级路径。
func (c *Chain) Capabilities() provider.Capabilities {
	for _, p := range c.candidates() {
		return p.Capabilities()
	}
	return provider.Capabilities{}
}

// Candidates 返回当前可用的 Provider 列表
func (c *Chain) candidates() []provider.Provider {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()

	out := make([]provider.Provider, 0, len(c.providers))
	for _, p := range c.providers {
		b := c.breakers[p.Name()]
		if b != nil && now.Before(b.openUntil) {
			continue // 熔断中，跳过
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		// 全部熔断 → 强制放行第一个，避免彻底不可用。
		// "宁可超时也不要无响应"是可用性优先的取舍。
		//
		// ★ 必须先判空：空链（配置里一个 Provider 都没有）时
		//   c.providers[:1] 会 panic("slice bounds out of range")。
		//   配置错误不该让进程崩溃，而该返回一条清晰的错误。
		if len(c.providers) == 0 {
			return nil
		}
		out = c.providers[:1]
	}
	return out
}

func (c *Chain) breaker(name string) *breaker {
	b, ok := c.breakers[name]
	if !ok {
		b = &breaker{}
		c.breakers[name] = b
	}
	return b
}

func (c *Chain) onSuccess(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.breakers[name] = &breaker{}
}

func (c *Chain) onFailure(name string, err error) {
	c.mu.Lock()
	b := c.breaker(name)
	b.failures++
	b.lastErr = err
	failures := b.failures
	tripped := failures >= c.th
	if tripped {
		b.openUntil = time.Now().Add(c.cooldown)
	}
	c.mu.Unlock()

	// ★ 日志必须在锁外。
	//
	//   slog handler 会做真实 I/O，高并发失败时会把所有 Chat 请求
	//   一起阻塞在互斥锁上 —— 监控日志的代价由业务请求承担。
	if tripped {
		logger.Warn(context.Background(), "provider 熔断",
			"provider", name, "failures", failures,
			"cooldown", c.cooldown.String(), "err", err)
	}
}

// Chat 按降级链调用
func (c *Chain) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	var lastErr error
	var degradeReason string

	for _, p := range c.candidates() {
		resp, err := p.Chat(ctx, req)
		if err == nil {
			c.onSuccess(p.Name())
			if degradeReason != "" {
				resp.Degraded = true
				resp.DegradeReason = degradeReason
			}
			return resp, nil
		}

		lastErr = err

		// ★ 先判 ctx，再记失败。
		//
		//   上游超时或客户端取消时，Provider 本身完全健康 ——
		//   若也计入熔断失败次数，一次网关抖动就可能把好Provider 熔断，
		//   后续正常请求全被降级。取消不属于 Provider 的责任。
		if ctx.Err() != nil {
			return nil, err
		}

		// ★ 能力不支持不算失败，必须在 onFailure 之前判定。
		//
		//   模型没有 Embed 能力是永久性的配置事实，不是故障。
		//   若计入熔断，请求 Embed 时这个模型会被反复探测
		//   直到攒够阈值被熔断 —— 每次探测都是一次注定失败的调用，
		//   白白消耗配额；而熔断期内所有 Embed 请求都被降级。
		if provider.IsUnsupported(err) {
			degradeReason = appendReason(degradeReason,
				fmt.Sprintf("%s 不支持该能力", p.Name()))
			logger.Info(ctx, "provider 能力不足，降级到下一个",
				"provider", p.Name(), "reason", err)
			continue
		}

		c.onFailure(p.Name(), err)

		degradeReason = appendReason(degradeReason,
			fmt.Sprintf("%s 调用失败(%v)", p.Name(), err))
		logger.Warn(ctx, "provider 调用失败，尝试降级",
			"provider", p.Name(), "err", err)
	}

	return nil, finalErr(lastErr)
}

// ChatStream 按降级链调用
func (c *Chain) ChatStream(ctx context.Context, req *provider.ChatRequest) (<-chan *provider.Chunk, error) {
	var lastErr error
	var degradeReason string

	for _, p := range c.candidates() {
		ch, err := p.ChatStream(ctx, req)
		if err == nil {
			c.onSuccess(p.Name())
			if degradeReason == "" {
				return ch, nil
			}
			// 需要在首帧注入降级标记：包一层
			return wrapDegrade(ctx, ch, degradeReason), nil
		}

		lastErr = err
		// 与 Chat 保持一致：ctx 取消不算 Provider 故障，能力不支持也不算
		if ctx.Err() != nil {
			return nil, err
		}
		if !provider.IsUnsupported(err) {
			c.onFailure(p.Name(), err)
		}

		degradeReason = appendReason(degradeReason,
			fmt.Sprintf("%s 流式不可用(%v)", p.Name(), err))
	}
	return nil, finalErr(lastErr)
}

// Rerank 逐个尝试，直到某个 Provider 成功。
//
// ★ 与 Embed 的一个关键区别：
//
//	全部 Provider 都不支持 rerank 时，返回 **ErrUnsupported** 而不是普通错误。
//
//	理由：rerank 是**可选增强**环节 —— 没有它检索链路照样能跑，
//	退化为「纯向量 + 关键词降级」。
//	若混进 lastErr 返回普通故障，上层会当调用失败去重试/熔断，
//	而它根本重试也不会好，属于典型的「不可恢复错误被当成可恢复错误」。
func (c *Chain) Rerank(ctx context.Context, req *provider.RerankRequest) (*provider.RerankResponse, error) {
	var lastErr error
	var degradeReason string
	unsupportedAll := true

	for _, p := range c.candidates() {
		resp, err := p.Rerank(ctx, req)
		if err == nil {
			c.onSuccess(p.Name())
			if degradeReason != "" {
				logger.Warn(ctx, "rerank 已降级",
					"from", degradeReason, "model", resp.Model)
			}
			return resp, nil
		}

		lastErr = err
		if ctx.Err() != nil {
			return nil, err
		}
		if provider.IsUnsupported(err) {
			// 能力缺失：不记为 Provider 故障，继续试下一个
			degradeReason = appendReason(degradeReason,
				fmt.Sprintf("%s 不支持 rerank", p.Name()))
			continue
		}

		// 真故障：记一次，并继续尝试后面的（可能有能力更好的）
		unsupportedAll = false
		c.onFailure(p.Name(), err)
		degradeReason = appendReason(degradeReason,
			fmt.Sprintf("%s rerank 失败(%v)", p.Name(), err))
	}

	if unsupportedAll {
		return nil, provider.ErrUnsupported
	}
	return nil, finalErr(lastErr)
}

// Embed 按降级链调用
func (c *Chain) Embed(ctx context.Context, req *provider.EmbedRequest) (*provider.EmbedResponse, error) {
	var lastErr error
	var degradeReason string

	for _, p := range c.candidates() {
		resp, err := p.Embed(ctx, req)
		if err == nil {
			c.onSuccess(p.Name())
			if degradeReason != "" {
				// Embed 没有 degraded 字段，通过 Model 名体现实际使用的模型
				logger.Warn(ctx, "embed 已降级",
					"from", degradeReason, "model", resp.Model)
			}
			return resp, nil
		}

		lastErr = err
		// ctx 取消与能力不支持都不算 Provider 故障（见 Chat 处说明）
		if ctx.Err() != nil {
			return nil, err
		}
		if !provider.IsUnsupported(err) {
			c.onFailure(p.Name(), err)
		}

		degradeReason = appendReason(degradeReason,
			fmt.Sprintf("%s embed 失败(%v)", p.Name(), err))
	}
	return nil, finalErr(lastErr)
}

// ListModels 合并链上所有模型
func (c *Chain) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	var all []provider.ModelInfo
	for _, p := range c.providers {
		ms, err := p.ListModels(ctx)
		if err != nil {
			logger.Warn(ctx, "list models 失败", "provider", p.Name(), "err", err)
			continue
		}
		all = append(all, ms...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("chain: 无可用 Provider")
	}
	return all, nil
}

// Close 关闭全部
func (c *Chain) Close() error {
	var firstErr error
	for _, p := range c.providers {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Health 返回各 Provider 的熔断状态（供健康检查暴露）
func (c *Chain) Health() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	out := make(map[string]string, len(c.breakers))
	for name, b := range c.breakers {
		switch {
		case b.openUntil.IsZero():
			out[name] = "ok"
		case now.Before(b.openUntil):
			out[name] = "open:" + b.openUntil.Format(time.RFC3339)
		default:
			out[name] = "half-open"
		}
	}
	return out
}

// wrapDegrade 在流式结果首帧注入降级标记。
//
// 用 goroutine 转发而非直接改写源 channel：源 channel 由下游 Provider 拥有，
// 我们不应写入别人的 channel。
//
// ★ 必须感知 ctx：
//
//	消费方（llm_service.ChatStream）在 stream.Send 失败时会直接 return，
//	不再读取返回的 channel。此时若本goroutine 阻塞在 `out <- chunk` 上，
//	就永远等不到读者 —— 每次客户端中途断开都会泄漏一个 goroutine。
//	select 的 ctx 分支让它能及时退出。
func wrapDegrade(ctx context.Context, src <-chan *provider.Chunk, reason string) <-chan *provider.Chunk {
	out := make(chan *provider.Chunk, 32)
	go func() {
		defer close(out)
		injected := false

		send := func(chunk *provider.Chunk) bool {
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for chunk := range src {
			if !injected {
				injected = true
				if !send(&provider.Chunk{Degraded: true, DegradeReason: reason}) {
					return
				}
			}
			if !send(chunk) {
				return
			}
		}
		// 源流异常结束时保证降级标记至少被发过一次
		if !injected {
			send(&provider.Chunk{Degraded: true, DegradeReason: reason})
		}
	}()
	return out
}

func appendReason(base, add string) string {
	if base == "" {
		return add
	}
	return base + "; " + add
}

// finalErr 兜底：candidates() 在空链时返回 nil，
// 此时 lastErr 也是 nil，直接返回会让上层拿到 (nil, nil) ——
// 既没有响应也没有错误，比 panic 更难排查。
func finalErr(last error) error {
	if last == nil {
		return errNoProviders
	}
	return last
}
