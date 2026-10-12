//go:build ignore

// Milvus 3.x 适配层真实连通性验证（不参与编译，手动执行）。
//
// 用途：部署前确认自研适配层（pkg/vectordb/milvus3）确实能用，
//
//	建集合 → 写入 → 同步 → 检索 → 阈值过滤 → 按ID查 → 删除 → 维度冲突保护
//
//	这条链路全部走真实 Milvus，任何一步不通都会立刻暴露。
//
// 用法：
//
//	source scripts/goenv.sh
//	set -a && source .env && set +a
//	go run scripts/milvus_probe.go
//
// 可选环境变量：
//
//	MILVUS_PROBE_COLLECTION   探测用集合名（默认 aegis_probe_tmp）
//	MILVUS_PROBE_KEEP=1       探测结束后**保留**集合（默认删除）
//
// ★ 为什么不让它成为常驻测试：
//
//	它需要一个真实 Milvus 实例，CI 里没有。
//	单测已覆盖不依赖实例的部分（过滤表达式、错误分类、Sync 降级），
//	这个脚本负责覆盖剩下那一半 —— 真机行为。
//
// ★ 为什么默认用临时集合名而不是 doc_chunks：
//
//	探测会建集合、反复写删。直接用业务集合名，
//	一次手滑的参数就可能污染线上数据。
//	万一真的想用它探测线上，务必设 MILVUS_PROBE_COLLECTION。
//
// ★ 为什么探测前要先清空集合：
//
//	Milvus 默认一致性等级 ClBounded，写入后立即检索可能召回 0 条
//	（且不报错）。若集合里留着上一次的向量，检索就会「看起来正常」——
//	召回的到底是本次写入还是历史数据，分不出来。
//	那样的测试等于没测。所以每次探测都用专属 kb_id 先清场，
//	确保断言的每一条都只能来自本次写入。
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/vectordb"
	"github.com/aegis/pkg/vectordb/milvus3"
)

// probeKBID 探测数据专用的 kb_id。
//
// ★ 为什么要用专属 kb_id 而不是随手写 1：
//
//	探测会反复写删同一个集合，若用业务可能真实存在的 kb_id，
//	清理动作（DeleteByKB）就可能删掉不属于探测的数据 ——
//	这个脚本默认集合名与业务不同，但一旦有人设了
//	MILVUS_PROBE_COLLECTION 指到业务集合，后果就是真实数据被删。
//	用一个几乎不可能与业务冲突的 id，把这个风险压到最低。
const probeKBID = 999999001

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cfg := loadConf()
	coll := envOr("MILVUS_PROBE_COLLECTION", "aegis_probe_tmp")
	keep := os.Getenv("MILVUS_PROBE_KEEP") == "1"
	dim := cfg.Dim
	if dim <= 0 {
		dim = 1024
	}

	fmt.Printf("目标 Milvus : %s (db=%s)\n", cfg.Addr, cfg.DBName)
	fmt.Printf("探测集合    : %s (dim=%d)\n\n", coll, dim)

	st, err := milvus3.New(ctx, cfg)
	must("连接 Milvus", err)
	defer func() { _ = st.Close() }()

	// ---------- 0. 探测前清场 ----------
	//
	// ★ 为什么要先清空集合，而不是直接复用残留数据：
	//
	//	最初版本的探测是「Upsert 后立即 Search」，
	//	第一次跑通过、第二次跑检索为空，
	//	一度被怀疑是探针有 bug 或Milvus 一致性问题。
	//	真正的原因是集合里留着上一次的向量 ——
	//	召回的到底是本次写入还是历史数据，肉眼分不出来。
	//
	//	对于要验证「写后立即可检索」的场景，
	//	**残留数据会让测试结果失去意义**：
	//	测的是「能不能搜到旧的」，不是「能不能搜到新的」。
	//
	//	用幂等的方式清：Upsert 前先按一个专用 kb_id 删除，
	//	这样无论集合是新建还是复用都是干净的。
	//
	//	集合不存在时要容忍 —— 首次探测就是这种情况，
	//	而它恰恰是最需要验证的场景（全新集合里没有任何残留）。
	//	直接 die 会让「在全新集合上探测」这条路永远走不通。
	if err := st.DeleteByKB(ctx, coll, probeKBID); err != nil {
		if !strings.Contains(err.Error(), "collection not found") {
			die(fmt.Sprintf("探测前清理集合失败: %v", err))
		}
		fmt.Printf("提示：集合 %s 尚不存在，将新建（首次探测的正常情形）\n", coll)
	}

	// ---------- 1. Ping ----------
	must("Ping", st.Ping(ctx))
	fmt.Println("✓ 1. Ping（探测前已清空集合，避免残留数据掩盖写入结果）")

	// ---------- 2. 建集合（幂等：已存在则校验维度） ----------
	must("EnsureCollection", st.EnsureCollection(ctx, coll, dim))
	fmt.Println("✓ 2. EnsureCollection")

	// ---------- 3. 维度冲突必须报错，不能静默重建 ----------
	// 这一步最关键：维度不符若被静默重建，
	// 业务库里已入库的向量会全部消失且无任何报错。
	if err := st.EnsureCollection(ctx, coll, dim+1); err == nil {
		fmt.Println("✗ 3. 维度冲突未被检出 —— 这是严重问题，已中止（未做清理）")
		os.Exit(1)
	}
	fmt.Println("✓ 3. 维度冲突被正确拒绝")

	// ---------- 4. 写入 ----------
	// 三个向量：前两个与查询向量同向，第三个正交。
	// 这样阈值过滤才有可验证的预期结果。
	vecs := probeVectors(dim)
	chunks := []*vectordb.Chunk{
		{ChunkID: "probe-1", KBID: probeKBID, DocID: 100, Seq: 0,
			Content: "RAG 是检索增强生成：先检索再让模型基于检索结果作答。",
			Vector:  vecs[0], PageNo: 1, TokenCount: 32, Tenant: "probe"},
		{ChunkID: "probe-2", KBID: probeKBID, DocID: 100, Seq: 1,
			Content: "向量检索通过计算 embedding 与查询向量的相似度排序召回结果。",
			Vector:  vecs[1], PageNo: 1, TokenCount: 32, Tenant: "probe"},
		{ChunkID: "probe-3", KBID: probeKBID, DocID: 101, Seq: 0,
			Content: "本文档内容与查询语义完全无关，用于验证阈值过滤。",
			Vector:  vecs[2], PageNo: 2, TokenCount: 32, Tenant: "probe"},
		// 无效行：ChunkID 为空，应被跳过而不是让整批失败
		{ChunkID: "", KBID: probeKBID, Vector: vecs[0]},
	}
	must("Upsert", st.Upsert(ctx, coll, chunks))
	fmt.Println("✓ 4. Upsert（4 条含 1 条无效行）")

	// ---------- 4.5 同步：让刚写入的数据立即可检索 ----------
	//
	// ★ 这一步不是多余的「保险」，而是必须验证的能力：
	//
	//   Milvus 默认ClBounded，写入后立即检索召回 0 条且**不报错**。
	//   若没有 Sync，第 5 步的检索结果就完全取决于运气——
	//   集合里恰好有旧数据时通过，全新集合上必然失败。
	//   那种「有时过有时不过」的测试等于没有测试。
	//
	//   所以下面刻意用**只可能来自本次写入的主键**来断言。
	if err := vectordb.SyncAfterWrite(ctx, st, coll); err != nil {
		die(fmt.Sprintf("Sync 失败: %v", err))
	}
	fmt.Println("✓ 4.5 Sync（Flush+Await，写后即可见）")

	// ---------- 5. 检索：阈值过滤 ----------
	q := normalize(vecs[0])
	res, err := st.Search(ctx, &vectordb.SearchRequest{
		Collection: coll, Vector: q, TopK: 10,
		ScoreThreshold: 0.5, // 只应召回前两条
		KBIDs:          []int64{probeKBID}, Tenant: "probe",
	})
	must("Search", err)
	if len(res.ChunkIDs) == 0 {
		die("检索结果为空 —— 阈值 0.5 下至少应有 probe-1/2 命中")
	}
	if len(res.ChunkIDs) != 2 {
		die(fmt.Sprintf("阈值 0.5 下召回了 %d 条（%v），期望恰好 2 条 —— 阈值过滤未生效",
			len(res.ChunkIDs), res.ChunkIDs))
	}
	fmt.Printf("✓ 5. Search + 阈值过滤（召回 %d 条: %v，分数 %.4f/%.4f）\n",
		len(res.ChunkIDs), res.ChunkIDs, res.Scores[res.ChunkIDs[0]], res.Scores[res.ChunkIDs[1]])

	// ---------- 6. 空结果不是故障 ----------
	res2, err := st.Search(ctx, &vectordb.SearchRequest{
		Collection: coll, Vector: q, TopK: 10,
		// 阈值 1.1 超出余弦上界 —— 必然零命中
		ScoreThreshold: 1.1,
		KBIDs:          []int64{probeKBID},
	})
	must("Search(超阈值不应报错)", err)
	if len(res2.ChunkIDs) != 0 {
		die(fmt.Sprintf("超阈值应返回空结果，实际召回 %v", res2.ChunkIDs))
	}
	fmt.Println("✓ 6. 零命中返回空结果而非错误")

	// ---------- 7. 维度错误的查询向量 ----------
	_, err = st.Search(ctx, &vectordb.SearchRequest{
		Collection: coll, Vector: make([]float32, dim+7), TopK: 5,
	})
	if err == nil {
		die("维度错误的查询向量应报错")
	}
	fmt.Printf("✓ 7. 查询向量维度不符被拒绝: %v\n", shortErr(err))

	// ---------- 8. 按 ID 查回 ----------
	got, err := st.QueryByIDs(ctx, coll, []string{"probe-1", "probe-3", "no-such-id"})
	must("QueryByIDs", err)
	if len(got) != 2 {
		die(fmt.Sprintf("QueryByIDs 应返回 2 条（不存在的 id 跳过），实际 %d", len(got)))
	}
	if len(got[0].Vector) != dim {
		die(fmt.Sprintf("回读的向量维度 %d，应为 %d", len(got[0].Vector), dim))
	}
	if got[0].Content == "" {
		die("回读的正文为空 —— output_fields 漏了 content")
	}
	fmt.Printf("✓ 8. QueryByIDs（2 条，向量维度 %d，正文非空）\n", len(got[0].Vector))

	// ---------- 9. Stats ----------
	// 注意：刚写入的数据不会立刻计入 Milvus 的 row_count
	//（需等 flush/compaction），所以 entities 很可能仍是 0 —— 正常现象。
	// 这里真正要验证的是 Stats 调用本身不报错、backend标识正确。
	stats, err := st.Stats(ctx, coll)
	must("Stats", err)
	if stats.Backend != "milvus3" {
		die(fmt.Sprintf("Stats.Backend = %q，期望 milvus3", stats.Backend))
	}
	fmt.Printf("✓ 9. Stats（backend=%s entities=%d healthy=%v；"+
		"entities 为 0 属正常：row_count 需等flush/compaction）\n",
		stats.Backend, stats.Entities, stats.Healthy)

	// ---------- 10. 清理 ----------
	if !keep {
		must("删除探测数据", st.DeleteByKB(ctx, coll, probeKBID))
		fmt.Println("✓ 10. DeleteByKB 清理完成")
		// 集合本身不删：留着不影响功能，下次探测幂等复用。
		// 删集合反而会制造「删掉正在被别处使用的集合」的风险。
		fmt.Printf("\n提示：集合 %s 保留（下次探测幂等复用），如需删除请手动执行", coll)
	} else {
		fmt.Printf("\n已按要求保留集合与数据: %s", coll)
	}

	fmt.Println("\n全部通过：自研 Milvus 3.x 适配层工作正常。")
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
	// 容器内运行时优先读 AEGIS_* 注入（与 compose 对齐）
	if v := os.Getenv("AEGIS_MILVUS__ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("AEGIS_MILVUS__DB_NAME"); v != "" {
		cfg.DBName = v
	}

	// ★ 把容器内主机名改写成宿主可达地址。
	//
	//   .env 里为了服务容器间通信写的是 MILVUS_ADDRESS=milvus:19530，
	//   但本脚本跑在宿主机上——直接用它会连不上（DNS 解析 milvus 失败），
	//   报出来却是 "context deadline exceeded"，
	//   看起来像「Milvus 没起来」，实际只是地址不对。这种误判很费时间。
	//
	//   只换主机名，端口与路径部分原样保留。
	for _, host := range []string{"milvus", "aegis-milvus"} {
		if strings.HasPrefix(cfg.Addr, host+":") {
			fixed := "127.0.0.1:" + strings.TrimPrefix(cfg.Addr, host+":")
			fmt.Printf("提示：地址 %s 是容器内主机名，已改写为 %s（宿主机可解析）\n\n",
				host, fixed)
			cfg.Addr = fixed
			break
		}
	}
	return cfg
}

// probeVectors 构造三组确定性向量。
//
// 用确定性而非随机：探测结果必须可复现，
// 否则「这次跑过了、下次跑不过」将无法区分是环境问题还是代码问题。
func probeVectors(dim int) [][]float32 {
	// a：全1 的单位向量
	a := make([]float32, dim)
	for i := range a {
		a[i] = 1
	}
	a = normalize(a)

	// b：与 a 同向但分布不同 —— 余弦约 0.99，应通过 0.5 阈值
	b := make([]float32, dim)
	for i := range b {
		b[i] = 0.5 + float32(i%7)/10
	}
	b = normalize(b)

	// c：与 a 正交 —— 余弦约 0，应被 0.5 阈值拦掉
	c := make([]float32, dim)
	for i := range c {
		c[i] = float32(math.Sin(float64(i)) + math.Cos(float64(i)))
	}
	// 若恰好与 a 正交，退化成第一维非零的向量保证正交
	if dot(a, c) > 0.01 {
		c = make([]float32, dim)
		c[0] = 1
		for i := 1; i < dim; i++ {
			c[i] = float32(math.Sin(float64(i * 7)))
		}
		c = normalize(c)
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

func dot(a, b []float32) float64 {
	var s float64
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

func must(step string, err error) {
	if err != nil {
		die(fmt.Sprintf("%s 失败: %v", step, err))
	}
}

func die(msg string) {
	fmt.Fprintf(os.Stderr, "\n✗ %s\n", msg)
	os.Exit(1)
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 110 {
		return s[:110] + "..."
	}
	return s
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
