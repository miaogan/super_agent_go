// Package usagestore 把 llm_service 的调用用量落到 PostgreSQL。
//
// 为什么需要它：UsageRecorder 原本只有进程内的环形缓冲（1000 条），
// 于是 ① 重启即丢全部历史；② 多副本各自一份，GetUsage 只能看到
// 恰好接到请求的那个副本 —— 用量统计的口径根本不成立。
//
// ★ 两条硬约束：
//  1. Save() 绝不可阻塞调用方。用量是旁路观测数据，让一次 PG 抖动
//     把对话请求一起拖死是优先级颠倒。因此走带缓冲的 channel，
//     满了就丢弃并计数。
//  2. 丢弃必须可见。静默丢数据等于没有数据 —— dropped 计数暴露给调用方，
//     由调用方打成指标/告警。
package usagestore

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/logger"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Record 一条待落库的用量。
//
// 字段与 token_usage 表一一对应；这里不复用 service.UsageRecord
// 是为了避免 service <-> usagestore 的循环依赖。
type Record struct {
	UID              int64
	AppID            string
	SessionID        string
	AgentID          string
	Model            string
	Provider         string
	PromptTokens     int32
	CompletionTokens int32
	TotalTokens      int32
	Cost             float64
	IsFallback       bool
	LatencyMS        int64
	CreatedAt        int64 // unix 秒；0 表示用库侧 now()
	RequestID        string
}

// Sink 用量落库出口。nil 实现者表示不落库（纯内存行为）。
type Sink interface {
	Save(rec *Record)
	Close(ctx context.Context) error
	// Dropped 累计因缓冲满/写失败而丢弃的条数
	Dropped() int64
}

var _ Sink = (*PG)(nil)

const insertSQL = "INSERT INTO token_usage " +
	"(uid, app_id, session_id, agent_id, model, provider, " +
	"prompt_tokens, completion_tokens, total_tokens, cost, " +
	"is_fallback, latency_ms, created_at) VALUES "

// PG 异步批量落库实现。
type PG struct {
	pool *pgxpool.Pool

	ch   chan *Record
	done chan struct{}
	wg   sync.WaitGroup

	dropped atomic.Int64
	closed  atomic.Bool

	// 批量与刷新参数
	batchMax  int
	flushIdle time.Duration

	// writeFn 实际的写入动作，默认 execInsert；抽出来是为了可测
	writeFn func(ctx context.Context, batch []*Record) error
}

// NewPG 建立连接池并启动后台写协程。
//
// DSN 为空或连接失败返回 error，调用方据此禁用落库（与 toolresolver
// 同一套降级姿势）：llm_service 不该因为"用量存不下"而启动失败。
func NewPG(ctx context.Context, pg conf.PostgresConf) (*PG, error) {
	if strings.TrimSpace(pg.DSN) == "" {
		return nil, fmt.Errorf("usagestore: postgres DSN 未配置")
	}
	cfg, err := pgxpool.ParseConfig(pg.DSN)
	if err != nil {
		return nil, fmt.Errorf("usagestore: 解析 DSN 失败: %w", err)
	}
	// 用量写入是旁路负载：池子给小一点，避免和主业务抢连接
	cfg.MaxConns = 3

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("usagestore: 建池失败: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("usagestore: ping 失败: %w", err)
	}

	p := &PG{
		pool:      pool,
		ch:        make(chan *Record, 4096),
		done:      make(chan struct{}),
		batchMax:  200,
		flushIdle: 500 * time.Millisecond,
	}
	p.writeFn = p.execInsert
	p.wg.Add(1)
	go p.run()
	return p, nil
}

// Save 投递一条用量。**永不阻塞**：channel 满时丢弃并计数。
func (p *PG) Save(rec *Record) {
	if rec == nil || p.closed.Load() {
		return
	}
	select {
	case p.ch <- rec:
	default:
		// 宁可丢观测数据，也不让对话请求陪着一起卡住
		n := p.dropped.Add(1)
		if n == 1 || n%1000 == 0 {
			logger.Warn(context.Background(), "用量落库缓冲已满，丢弃中",
				"dropped_total", n, "buffer", cap(p.ch))
		}
	}
}

// Dropped 累计丢弃条数。
func (p *PG) Dropped() int64 { return p.dropped.Load() }

// Close 停止接收、尽力把缓冲区写干净。
func (p *PG) Close(ctx context.Context) error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(p.ch) // run() 会读完剩余再退出
	waited := make(chan struct{})
	go func() { p.wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-ctx.Done():
		logger.Warn(ctx, "用量落库未在期限内排空，放弃剩余缓冲条目",
			"dropped_total", p.dropped.Load())
	}
	// 池子可能尚未建立（构造失败或测试用的裸实例），
	// 无守卫时 (*pgxpool.Pool)(nil).Close() 会直接 panic。
	if p.pool != nil {
		p.pool.Close()
	}
	return nil
}

// run 后台批写协程。
func (p *PG) run() {
	defer p.wg.Done()
	timer := time.NewTimer(p.flushIdle)
	defer timer.Stop()

	batch := make([]*Record, 0, p.batchMax)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		p.writeBatch(batch)
		batch = batch[:0]
	}
	for {
		select {
		case rec, ok := <-p.ch:
			if !ok { // Save 已关闭，排空后退出
				flush()
				return
			}
			batch = append(batch, rec)
			if len(batch) >= p.batchMax {
				flush()
				timer.Reset(p.flushIdle)
			}
		case <-timer.C:
			flush()
			timer.Reset(p.flushIdle)
		}
	}
}

// writeBatch 把一批用量写进 PG。
//
// 失败时整批计入 dropped —— 不重试：用量数据允许少量缺失，
// 而为它引入重试就得再引入队列上限与退避，代价不成比例。
//
// 走 writeFn 而不是直接碰 pool，是为了让"Close 时排空缓冲区"这类
// 行为能在不连库的情况下被测到。
func (p *PG) writeBatch(batch []*Record) {
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := p.writeFn(ctx, batch); err != nil {
		n := p.dropped.Add(int64(len(batch)))
		logger.Warn(ctx, "用量批量落库失败，本批丢弃",
			"rows", len(batch), "dropped_total", n, "err", err)
	}
}

// buildInsert 拼多行 VALUES 的 INSERT 与对应参数。
//
// 单独成纯函数是为了能测"参数与占位符是否错位"——这类问题只有真的
// 连上库才会暴露，而错位的后果是整批写失败、用量静默丢光。
//
// 列顺序：uid, app_id, session_id, agent_id, model, provider,
//
//	prompt_tokens, completion_tokens, total_tokens, cost,
//	is_fallback, latency_ms, created_at
func buildInsert(batch []*Record) (string, []any) {
	const cols = 13

	var sb strings.Builder
	sb.WriteString(insertSQL)
	args := make([]any, 0, len(batch)*cols)
	for i, r := range batch {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('(')
		for j := 0; j < cols; j++ {
			if j > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "$%d", i*cols+j+1)
		}
		sb.WriteByte(')')

		created := time.Now()
		if r.CreatedAt > 0 {
			created = time.Unix(r.CreatedAt, 0)
		}
		// 调用方常只填 prompt/completion，total 缺省时在此推导，
		// 否则表里的 total_tokens 会全是 0，用量报表直接失真。
		total := r.TotalTokens
		if total == 0 {
			total = r.PromptTokens + r.CompletionTokens
		}
		args = append(args,
			r.UID, r.AppID, r.SessionID, r.AgentID, r.Model, r.Provider,
			r.PromptTokens, r.CompletionTokens, total, r.Cost,
			r.IsFallback, r.LatencyMS, created)
	}
	return sb.String(), args
}

// execInsert 真实写库实现（默认 writeFn）。
func (p *PG) execInsert(ctx context.Context, batch []*Record) error {
	query, args := buildInsert(batch)
	_, err := p.pool.Exec(ctx, query, args...)
	return err
}
