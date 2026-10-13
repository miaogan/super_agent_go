package usagestore

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aegis/pkg/conf"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNewPGValidatesConfig(t *testing.T) {
	ctx := context.Background()
	cases := map[string]string{
		"空 DSN":    "",
		"纯空白":      "   ",
		"非法连接串":    "not-a-dsn",
		"协议不支持":    "mysql://u:p@127.0.0.1:3306/db",
		"不可达地址":    "postgres://u:p@127.0.0.1:1/postgres?sslmode=disable",
		"DNS失败":    "postgres://u:p@no-such-host.invalid:5432/db?sslmode=disable",
		"端口非数字":    "postgres://u:p@127.0.0.1:abc/db",
		"percent错": "postgres://u:%zz@127.0.0.1:5432/db",
	}
	for name, dsn := range cases {
		t.Run(name, func(t *testing.T) {
			// 配置非法或连不上都必须返回 error，让调用方走降级；
			// 绝不能返回一个"看着可用、实际永不落库"的 sink。
			s, err := NewPG(ctx, conf.PostgresConf{DSN: dsn})
			if err == nil {
				if s != nil {
					_ = s.Close(context.Background())
				}
				t.Fatalf("DSN=%q 应返回 error", dsn)
			}
			if s != nil {
				t.Fatal("返回 error 时 sink 必须为 nil")
			}
		})
	}
}

// TestSaveNeverBlocksWhenBufferFull ★ Save 绝不能拖慢对话请求。
//
// 用量是旁路观测数据：为它阻塞请求，等于让一次 PG 抖动放大成整条链路变慢。
// 这里不连库，直接构造满缓冲的裸结构验证语义。
func TestSaveNeverBlocksWhenBufferFull(t *testing.T) {
	p := &PG{ch: make(chan *Record, 2), batchMax: 200, flushIdle: time.Hour}

	p.Save(&Record{UID: 1})
	p.Save(&Record{UID: 2})

	start := time.Now()
	p.Save(&Record{UID: 3}) // 缓冲已满
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("缓冲满时 Save 阻塞了 %v，应立即返回并丢弃", elapsed)
	}
	if got := p.Dropped(); got != 1 {
		t.Errorf("丢弃必须计数, 实际 dropped=%d", got)
	}
	if len(p.ch) != 2 {
		t.Errorf("缓冲区应保持满, 实际 %d", len(p.ch))
	}
}

// TestSaveIgnoresClosedAndNil 关闭后或 nil 记录都不应入队、不应 panic。
func TestSaveIgnoresClosedAndNil(t *testing.T) {
	p := &PG{ch: make(chan *Record, 4), batchMax: 10, flushIdle: time.Hour}
	p.closed.Store(true)

	p.Save(nil)
	p.Save(&Record{UID: 1})
	if len(p.ch) != 0 {
		t.Errorf("已关闭不应再入队, 实际 %d", len(p.ch))
	}
}

// TestBuildInsertShape 检查多行 INSERT 的占位符与参数是否一一对齐。
//
// 错位只有连上库才暴露，而后果是整批写失败、用量静默丢光。
func TestBuildInsertShape(t *testing.T) {
	batch := []*Record{
		{UID: 1, Model: "m1", PromptTokens: 3, CompletionTokens: 4},
		{UID: 2, Model: "m2", PromptTokens: 1, CompletionTokens: 1, TotalTokens: 9},
	}
	query, args := buildInsert(batch)

	const cols = 13
	if !strings.HasPrefix(query, "INSERT INTO token_usage") {
		t.Errorf("SQL 前缀不对: %.60q", query)
	}
	if got := strings.Count(query, "$"); got != cols*len(batch) {
		t.Errorf("占位符数应为 %d, 实际 %d", cols*len(batch), got)
	}
	if len(args) != cols*len(batch) {
		t.Errorf("参数个数应为 %d, 实际 %d", cols*len(batch), len(args))
	}
	// 最后一行占位符必须续上全局序号，不能两行都从 $1 开始
	if !strings.Contains(query, "$14") {
		t.Errorf("第二行占位符应从 $14 起, SQL=%s", query)
	}
	// total_tokens：缺省时由 prompt+completion 推导；显式值不被覆盖
	if got := args[8].(int32); got != 7 {
		t.Errorf("第一行 total 应推导为 7, 实际 %d", got)
	}
	if got := args[cols+8].(int32); got != 9 {
		t.Errorf("显式 TotalTokens=9 不应被覆盖, 实际 %d", got)
	}
	// created_at 必须是 time.Time（列是 timestamptz，传整数会直接类型报错）
	if _, ok := args[cols+12].(time.Time); !ok {
		t.Errorf("created_at 参数应为 time.Time, 实际 %T", args[cols+12])
	}
}

// TestWriteBatchCountsDroppedOnFailure 写失败必须整批计入 dropped。
func TestWriteBatchCountsDroppedOnFailure(t *testing.T) {
	p := &PG{ch: make(chan *Record, 8), batchMax: 8, flushIdle: time.Hour}
	p.writeFn = func(context.Context, []*Record) error { return context.DeadlineExceeded }

	p.writeBatch([]*Record{{UID: 1}, {UID: 2}, {UID: 3}})
	if got := p.Dropped(); got != 3 {
		t.Errorf("整批 3 条都应计为丢弃, 实际 %d", got)
	}

	// 空批次不应触发写入，也不应改变计数
	called := 0
	p.writeFn = func(context.Context, []*Record) error { called++; return nil }
	p.writeBatch(nil)
	if called != 0 || p.Dropped() != 3 {
		t.Errorf("空批次不应写库或改变计数: called=%d dropped=%d", called, p.Dropped())
	}
}

// TestRunDrainsBufferOnClose 退出路径必须排空缓冲区，
// 否则每次滚动重启都会丢掉最后一批用量。
func TestRunDrainsBufferOnClose(t *testing.T) {
	var flushed int
	p := &PG{ch: make(chan *Record, 16), batchMax: 100, flushIdle: time.Hour}
	p.writeFn = func(_ context.Context, batch []*Record) error {
		flushed += len(batch)
		return nil
	}
	p.wg.Add(1)
	go p.run()

	for i := 0; i < 5; i++ {
		p.Save(&Record{UID: int64(i)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if flushed != 5 {
		t.Errorf("Close 应排空 5 条, 实际落库 %d", flushed)
	}
}

// TestCloseIsIdempotent 二次 Close 不应 panic（重复 close channel）。
func TestCloseIsIdempotent(t *testing.T) {
	p := &PG{ch: make(chan *Record, 4), batchMax: 4, flushIdle: time.Millisecond}
	p.writeFn = func(context.Context, []*Record) error { return nil }
	p.wg.Add(1)
	go p.run()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		t.Fatalf("首次 Close 失败: %v", err)
	}
	if err := p.Close(ctx); err != nil {
		t.Errorf("二次 Close 应无害, 实际 %v", err)
	}
}

// TestSaveAfterCloseDoesNotPanic 关闭后再 Save 不能向已关闭 channel 发送。
func TestSaveAfterCloseDoesNotPanic(t *testing.T) {
	p := &PG{ch: make(chan *Record, 4), batchMax: 4, flushIdle: time.Millisecond}
	p.writeFn = func(context.Context, []*Record) error { return nil }
	p.wg.Add(1)
	go p.run()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	p.Save(&Record{UID: 1}) // 这里 panic 会让整个测试包失败
}

// TestPGWritesToRealDatabase 端到端验证：真的把行写进 token_usage 并读回来。
//
// 默认跳过，设 AEGIS_TEST_PG_DSN 才跑（CI 里没有库；本地起
// `docker compose up -d postgres` 后带上 DSN 即可）。
// 之所以愿意留这个门控测试：本包的其余用例都在验证"不连库的语义"，
// 而占位符/列序/分区表这些恰恰只有真库能证伪。
func TestPGWritesToRealDatabase(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("AEGIS_TEST_PG_DSN"))
	if dsn == "" {
		t.Skip("未设置 AEGIS_TEST_PG_DSN，跳过真实库写入验证")
	}
	ctx := context.Background()
	p, err := NewPG(ctx, conf.PostgresConf{DSN: dsn})
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}

	probeUID := time.Now().UnixNano() % 1_000_000_000
	p.Save(&Record{
		UID: probeUID, AppID: "probe", Model: "probe-model", Provider: "rawhttp",
		PromptTokens: 11, CompletionTokens: 7, Cost: 0.25, IsFallback: true, LatencyMS: 42,
	})
	// Close 会排空缓冲区，比 sleep 等定时器可靠
	if err := p.Close(ctx); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	back, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("回读建池失败: %v", err)
	}
	defer back.Close()

	var (
		gotTotal int32
		gotFall  bool
		gotProv  string
		gotCost  float64
	)
	err = back.QueryRow(ctx,
		"SELECT total_tokens, is_fallback, provider, cost FROM token_usage WHERE uid=$1",
		probeUID).Scan(&gotTotal, &gotFall, &gotProv, &gotCost)
	if err != nil {
		t.Fatalf("读回落库行失败（行未写入？）: %v", err)
	}
	if gotTotal != 18 {
		t.Errorf("total_tokens 应为 11+7=18, 实际 %d", gotTotal)
	}
	if !gotFall || gotProv != "rawhttp" {
		t.Errorf("is_fallback/provider 不符: %v %q", gotFall, gotProv)
	}
	if _, err := back.Exec(ctx, "DELETE FROM token_usage WHERE uid=$1", probeUID); err != nil {
		t.Logf("清理探针行失败（不影响结论）: %v", err)
	}
}
