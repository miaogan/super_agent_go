//go:build ignore

// Milvus 一致性行为诊断（不参与编译，手动执行）。
//
// ★ 为什么需要这个脚本：
//
//	探测脚本 milvus_probe.go 第一次跑通、第二次跑「检索结果为空」。
//	表面看是探针有bug，实际上暴露了两个完全不同的候选根因：
//	  A. tombstone：第一次结束时 DeleteByKB 删了数据，
//	     第二次 Upsert 相同主键可能命中尚未生效的删除标记
//	  B. Bounded 一致性：Milvus 默认 ClBounded，
//	     新写入的数据在 flush 前对 Search 不可见
//
//	两者的修法完全不同（A改主键策略 / B 改一致性级别或主动 flush），
//	凭猜改会引入新的坑 —— 所以用实验区分。
//
// 实验矩阵（每步都立即检索，记录是否命中）：
//
//	E1 新建集合 → 写 A/B/C → 立即检索        基线，首次写入
//	E2 删除全部 → 写**新主键** D/E/F → 立即检索   区分：是否与「同主键」有关
//	E3 再写**同主键** A/B/C → 立即检索          区分：是否 tombstone
//	E4 等待 1s → 检索E3 的数据            区分：是否只是延迟
//	E5 等待 5s → 检索 E3 的数据
//	E6 用 ClStrong 检索 E3 的数据             区分：是否可通过一致性级别解决
//
// 若 E3 空而 E6 有结果 → 根因是 B（Bounded 一致性）
// 若 E3 空且E6 也空、但 E5 有结果 → 纯延迟问题（回滚/重试可覆盖）
// 若 E2 也空 → 与主键无关，是写入可见性问题
//
// 用法：
//
//	source scripts/goenv.sh
//	set -a && source .env && set +a
//	go run scripts/milvus_diag.go
//
// 可选环境变量：
//
//	MILVUS_DIAG_COLLECTION   诊断集合名（默认 aegis_diag_tmp）
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/vectordb"
	"github.com/aegis/pkg/vectordb/milvus3"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg := loadConf()
	coll := envOr("MILVUS_DIAG_COLLECTION", "aegis_diag_tmp")
	dim := cfg.Dim
	if dim <= 0 {
		dim = 1024
	}

	fmt.Printf("目标 Milvus : %s (db=%s)\n", cfg.Addr, cfg.DBName)
	fmt.Printf("诊断集合    : %s (dim=%d)\n\n", coll, dim)

	st, err := milvus3.New(ctx, cfg)
	must("连接 Milvus", err)
	defer func() { _ = st.Close() }()

	// 每次诊断用全新集合，避开历史 tombstone 干扰。
	// ★ tsTag() 只能调一次：两次调用会跨秒/跨格式化边界产出不同后缀，
	//   那样建集合用的名字和后续操作用的名字就对不上了，
	//   症状是「写进去了但查不到」—— 恰好和要诊断的问题一模一样。
	//   这类「诊断脚本自己制造假阳性」的情况最难查，必须先排除。
	uniqueColl := coll + "_" + tsTag()
	must("新建集合", st.EnsureCollection(ctx, uniqueColl, dim))
	fmt.Printf("实际诊断集合: %s\n\n", uniqueColl)

	vecs := probeVectors(dim)
	q := normalize(vecs[0])

	// ---------- E1 首次写入 ----------
	must("E1 Upsert", st.Upsert(ctx, uniqueColl, mkChunks("a", dim, vecs, 11)))
	e1 := trySearch(ctx, st, uniqueColl, q, "a-1")
	fmt.Printf("E1 首次写入立即检索        : %s\n\n", e1)

	// ---------- E2 删除后写新主键 ----------
	must("E2 删除", st.DeleteByKB(ctx, uniqueColl, 11))
	must("E2 Upsert新主键", st.Upsert(ctx, uniqueColl, mkChunks("b", dim, vecs, 12)))
	e2 := trySearch(ctx, st, uniqueColl, q, "b-1")
	fmt.Printf("E2 删后写新主键立即检索    : %s\n", e2)
	fmt.Printf("   （若 E1 有结果而 E2 空 → 问题与「同主键/tombstone」无关）\n\n")

	// ---------- E3 再写同主键 a ----------
	must("E3 删除", st.DeleteByKB(ctx, uniqueColl, 12))
	must("E3 Upsert 同主键 a", st.Upsert(ctx, uniqueColl, mkChunks("a", dim, vecs, 13)))
	e3 := trySearch(ctx, st, uniqueColl, q, "a-1")
	fmt.Printf("E3 写同主键立即检索        : %s\n", e3)
	fmt.Printf("   （若 E2 空而 E3 空 → 纯写入可见性问题；\n")
	fmt.Printf("     若 E2 有结果而 E3 空 → 确认是 tombstone）\n\n")

	// ---------- E4/E5 等待后重试 ----------
	time.Sleep(1 * time.Second)
	e4 := trySearch(ctx, st, uniqueColl, q, "a-1")
	fmt.Printf("E4 等 1s 后重试: %s\n", e4)

	time.Sleep(4 * time.Second)
	e5 := trySearch(ctx, st, uniqueColl, q, "a-1")
	fmt.Printf("E5 等 5s 后重试            : %s\n", e5)

	// ---------- E6 强一致性 ----------
	e6 := trySearchStrong(ctx, cfg, uniqueColl, q, "a-1")
	fmt.Printf("E6 ClStrong 检索           : %s\n\n", e6)

	// ---------- E7/E8 Flush 能否解决，代价多大 ----------
	//
	// 这是决定「要不要在 Upsert 里加 Flush」的关键数据：
	// 只知道 Flush 能解决没用，得知道它有多贵。
	e7, flushCost := testFlush(ctx, cfg, uniqueColl, q, "z-1")
	fmt.Printf("E7 Upsert+Flush 后立即检索 : %s（Flush+Await 耗时 %v）\n", e7, flushCost)
	fmt.Printf("   （若 E7 立即可见 → Flush 是可行的确定性方案，代价见上）\n\n")

	// ---------- 结论 ----------
	fmt.Println("========== 结论 ==========")
	switch {
	case e7.hit:
		fmt.Println("根因 = Bounded 一致性下新写入数据尚未对检索可见")
		fmt.Printf("   Flush+Await 耗时 %v，且 Flush 后立即可检索 → 可行方案\n", flushCost)
		fmt.Println("建议：Upsert 提供「写后可见」选项（可配置），")
		fmt.Println("      在入库流水线开启 —— 让「上传完立刻能搜到」成为确定行为")
	case e2.hit && !e3.hit:
		fmt.Println("根因 = tombstone（删除标记未生效，重写同主键被吞）")
		fmt.Println("对策：写入前后避免同主键重写；或对删除做延迟校验；")
		fmt.Println("      或查询侧放宽（当前实现已用 Upsert 而非 Insert，方向正确）")
	case e3.hit:
		fmt.Println("根因 = 短暂延迟（5s 内自愈），非阻塞缺陷")
		fmt.Println("对策：入库流水线在「刚写完立即查」场景加短暂重试即可")
	case e6.hit:
		fmt.Println("根因 = Bounded 一致性（ClStrong 可见，ClBounded 不可见）")
		fmt.Println("对策：Search 设置 WithConsistencyLevel(entity.ClStrong)")
	default:
		fmt.Println("未复现：所有实验都未命中 ——")
		fmt.Println("请检查是否 Collection 已 load、kb_id 过滤条件、维度是否匹配")
	}
	fmt.Println("=========================")

	// 清理
	if err := st.DeleteByKB(ctx, uniqueColl, 11); err != nil {
		_ = err
	}
	_ = st.DeleteByKB(ctx, uniqueColl, 12)
	_ = st.DeleteByKB(ctx, uniqueColl, 13)
	fmt.Printf("\n诊断集合 %s 保留（需手动删除集合）", uniqueColl)
}

type probeResult struct {
	hit   bool
	ids   []string
	score float64
}

func (r probeResult) String() string {
	if !r.hit {
		return "空"
	}
	return fmt.Sprintf("命中 %d 条 %v 最高分 %.4f", len(r.ids), r.ids, r.score)
}

func trySearch(ctx context.Context, st *milvus3.Store,
	coll string, q []float32, want string) probeResult {
	res, err := st.Search(ctx, &vectordb.SearchRequest{
		Collection: coll, Vector: q, TopK: 10,
		ScoreThreshold: 0.5, Tenant: "diag",
	})
	if err != nil {
		return probeResult{}
	}
	for i, id := range res.ChunkIDs {
		if id == want {
			return probeResult{hit: true, ids: res.ChunkIDs, score: res.Scores[id]}
		}
		_ = i
	}
	if len(res.ChunkIDs) > 0 {
		return probeResult{hit: false, ids: res.ChunkIDs}
	}
	return probeResult{}
}

// testFlush 验证「Upsert 后主动 Flush 再检索」能否消除可见性延迟，
// 并返回 Flush+Await 的耗时 —— 这是评估该方案代价的关键指标。
//
// ★ 为什么必须同时测「能不能」和「贵不贵」：
//
//	知道 Flush 能解决问题却不知道它要几百毫秒，就直接给入库流水线
//	每批加 Flush，那是拿吞吐换一致性 —— 而RAG 入库本来就是批量操作，
//	每批一次 Flush 落在百毫秒量级是可接受的，完全不是问题。
//	但如果不测就拍脑袋加，可能把写入延迟拉高一个数量级。
func testFlush(ctx context.Context, cfg conf.MilvusConf,
	coll string, q []float32, want string) (probeResult, time.Duration) {
	cli, err := sdkConnect(ctx, cfg)
	if err != nil {
		fmt.Printf("   [E7 跳过：SDK 直连失败 %v]\n", shortErr(err))
		return probeResult{}, 0
	}
	defer func() { _ = cli.Close(context.Background()) }()

	// 先写一条带唯一主键的数据
	vecs := probeVectors(cfg.Dim)
	row := map[string]any{
		"chunk_id":    want,
		"kb_id":       int64(77),
		"doc_id":      int64(7700),
		"seq":         int64(0),
		"content":     "Flush 可见性验证切片",
		"page_no":     int64(1),
		"token_count": int64(16),
		"tenant":      "diag",
	}
	insertOpt := milvusclient.NewColumnBasedInsertOption(coll).
		WithVarcharColumn("chunk_id", []string{want}).
		WithInt64Column("kb_id", []int64{77}).
		WithInt64Column("doc_id", []int64{7700}).
		WithInt64Column("seq", []int64{0}).
		WithVarcharColumn("content", []string{"Flush 可见性验证切片"}).
		WithInt64Column("page_no", []int64{1}).
		WithInt64Column("token_count", []int64{16}).
		WithVarcharColumn("tenant", []string{"diag"}).
		WithFloatVectorColumn("vector", cfg.Dim, [][]float32{vecs[0]})
	_ = row

	ictx, icancel := context.WithTimeout(ctx, 30*time.Second)
	if _, err := cli.Insert(ictx, insertOpt); err != nil {
		icancel()
		fmt.Printf("   [E7 Insert 失败: %v]\n", shortErr(err))
		return probeResult{}, 0
	}
	icancel()

	// Flush + Await：等真正落盘后才算完成
	start := time.Now()
	fctx, fcancel := context.WithTimeout(ctx, 60*time.Second)
	defer fcancel()
	task, err := cli.Flush(fctx, milvusclient.NewFlushOption(coll))
	if err == nil {
		err = task.Await(fctx)
	}
	cost := time.Since(start)
	if err != nil {
		fmt.Printf("   [E7 Flush 失败: %v]\n", shortErr(err))
		return probeResult{}, cost
	}

	// Flush 完成后立即用默认（ClBounded）检索
	opt := milvusclient.NewSearchOption(coll, 5, []entity.Vector{entity.FloatVector(q)}).
		WithOutputFields("chunk_id").
		WithFilter(`(tenant == "" or tenant == "diag")`)
	sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
	defer scancel()
	sets, err := cli.Search(sctx, opt)
	if err != nil {
		return probeResult{}, cost
	}
	return readResultSets(sets, want), cost
}

// trySearchStrong 用强一致性（ClStrong）直接走 SDK 检索，
// 用来区分「Bounded 看不到」与「数据真的不在」。
//
// ★ 为什么必须单独建一条 SDK 连接而不是复用 st.Search：
//
//	适配层的 Search 固定用集合默认的一致性级别，
//	没有暴露一致性开关 —— 这是有意的（一致性级别是部署级策略，
//	不该由每次调用随意指定）。
//	所以要验证「换成Strong 就能看到」，只能绕过适配层直接调 SDK。
//	这个函数是诊断专用，不构成「适配层应该加开关」的依据。
func trySearchStrong(ctx context.Context, cfg conf.MilvusConf,
	coll string, q []float32, want string) probeResult {
	cli, err := sdkConnect(ctx, cfg)
	if err != nil {
		fmt.Printf("   [E6跳过：SDK 直连失败 %v]\n", shortErr(err))
		return probeResult{}
	}
	defer func() { _ = cli.Close(context.Background()) }()

	// 与适配层同样的过滤条件：tenant 兼容空值。
	// 注意这里必须自己拼 —— 适配层的 buildFilter 是非导出的。
	expr := fmt.Sprintf(`(tenant == "" or tenant == "diag")`)

	opt := milvusclient.NewSearchOption(coll, 10, []entity.Vector{entity.FloatVector(q)}).
		WithOutputFields("chunk_id").
		WithFilter(expr).
		WithConsistencyLevel(entity.ClStrong)

	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	sets, err := cli.Search(sctx, opt)
	if err != nil {
		fmt.Printf("   [E6 Strong 检索报错: %v]\n", shortErr(err))
		return probeResult{}
	}
	return readResultSets(sets, want)
}

// sdkConnect 复刻适配层的两阶段连接（SDK 直连，不走适配层）。
func sdkConnect(ctx context.Context, cfg conf.MilvusConf) (*milvusclient.Client, error) {
	cli, err := milvusclient.New(ctx, &milvusclient.ClientConfig{
		Address:  cfg.Addr,
		Username: cfg.Username,
		Password: cfg.Password,
		// DBName 留空：SDK 建连时会校验库是否存在，
		// 与适配层同样的理由（见 store.go 注释）。
	})
	if err != nil {
		return nil, err
	}
	if cfg.DBName != "" {
		if err := cli.UseDatabase(ctx, milvusclient.NewUseDatabaseOption(cfg.DBName)); err != nil {
			_ = cli.Close(context.Background())
			return nil, err
		}
	}
	return cli, nil
}

// readResultSets 从检索结果里找目标 chunk_id。
func readResultSets(sets []milvusclient.ResultSet, want string) probeResult {
	if len(sets) == 0 {
		return probeResult{}
	}
	rs := sets[0]
	out := probeResult{}
	for i := 0; i < rs.Len(); i++ {
		id, err := rs.IDs.GetAsString(i)
		if err != nil || id == "" {
			continue
		}
		out.ids = append(out.ids, id)
		if id == want && !out.hit {
			out.hit = true
			out.score = float64(rs.Scores[i])
		}
	}
	return out
}

// mkChunks 构造一组同前缀的 chunk（kbID 用于区分批次）
func mkChunks(prefix string, dim int, vecs [][]float32, kbID int64) []*vectordb.Chunk {
	out := make([]*vectordb.Chunk, 0, 3)
	for i, v := range vecs {
		out = append(out, &vectordb.Chunk{
			ChunkID:    fmt.Sprintf("%s-%d", prefix, i+1),
			KBID:       kbID,
			DocID:      kbID * 100,
			Seq:        int32(i),
			Content:    fmt.Sprintf("诊断用切片 %s-%d，内容需足够长以避免空字段", prefix, i+1),
			Vector:     v,
			PageNo:     1,
			TokenCount: 32,
			Tenant:     "diag",
		})
	}
	return out
}

func tsTag() string {
	return time.Now().Format("150405")
}

func loadConf() conf.MilvusConf {
	cfg := conf.MilvusConf{
		Addr:               envOr("MILVUS_ADDRESS", "127.0.0.1:19530"),
		Username:           os.Getenv("MILVUS_USERNAME"),
		Password:           os.Getenv("MILVUS_PASSWORD"),
		DBName:             envOr("MILVUS_DB", "aegis"),
		Dim:                1024,
		MetricType:         "COSINE",
		Timeout:            30 * time.Second,
		HNSWEf:             64,
		HNSWEfConstruction: 200,
		HNSWM:              16,
	}
	if v := os.Getenv("AEGIS_MILVUS__ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("AEGIS_MILVUS__DB_NAME"); v != "" {
		cfg.DBName = v
	}
	for _, host := range []string{"milvus", "aegis-milvus"} {
		if strings.HasPrefix(cfg.Addr, host+":") {
			cfg.Addr = "127.0.0.1:" + strings.TrimPrefix(cfg.Addr, host+":")
			break
		}
	}
	return cfg
}

func probeVectors(dim int) [][]float32 {
	a := make([]float32, dim)
	for i := range a {
		a[i] = 1
	}
	a = normalize(a)

	b := make([]float32, dim)
	for i := range b {
		b[i] = 0.5 + float32(i%7)/10
	}
	b = normalize(b)

	c := make([]float32, dim)
	for i := range c {
		c[i] = float32(math.Sin(float64(i)) + math.Cos(float64(i)))
	}
	return [][]float32{a, b, c}
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

func must(step string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ %s 失败: %v\n", step, err)
		os.Exit(1)
	}
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
