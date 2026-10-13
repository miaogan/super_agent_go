//go:build ignore

// 存量向量 tenant 回填规模巡检（**只读**）。
//
// ★ 为什么需要它：
//
//	向量侧的租户过滤条件是 `(tenant == "" or tenant == <请求租户>)`，
//	而 ingest 直到最近才开始写入 Tenant（见 rag_service/internal/ingest/pipeline.go）。
//	保留 `tenant == ""` 那一支是为了不让改动前入库的存量切片从检索结果里
//	静默消失 —— 但代价是：在这些行被回填之前，这条过滤不构成租户隔离。
//
//	要决定是否收紧成等值过滤，先得知道存量里到底有多少行 tenant 为空。
//	本脚本就是量这个数的。
//
// 只读保证：全流程只调用 ListCollections / GetCollectionStats / Get(查询)，
// 不含任何 Upsert/Delete/CreateIndex/Compact。可以直接对生产库运行。
//
// 用法：
//
//	source scripts/goenv.sh
//	set -a && source .env && set +a
//	go run scripts/tenant_backfill_audit.go
//
// 可选环境变量：
//
//	MILVUS_DB           库名（默认 aegis）
//	AEGIS_MILVUS__ADDR  Milvus 地址（与 compose 注入对齐）
//	TENANT_AUDIT_PREFIX 集合名前缀（默认 aegis_kb_）
//	TENANT_AUDIT_KBIDS  只查指定 kb_id，逗号分隔（默认全部）
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"github.com/aegis/pkg/conf"
)

// schema 字段名。与 pkg/vectordb/milvus3/store.go:58-67 的常量必须一致，
// 那边是未导出常量，这里只能重复一次；改那边请同步改这里。
const (
	fieldTenant  = "tenant"
	fieldChunkID = "chunk_id"

	// Milvus 单次查询的返回上限。命中数等于它时真实值可能更大，
	// 必须标出来，否则报出去的数会被当成精确值用。
	queryHardLimit = 16384
)

// row 单个集合的巡检结果。
//
// 声明在包级而非 main 内部：countCell 需要它。
type row struct {
	coll    string
	total   int64
	empty   int
	capped  bool
	errText string
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg := loadConf()
	prefix := envOr("TENANT_AUDIT_PREFIX", "aegis_kb_")

	fmt.Printf("目标 Milvus : %s (db=%s)\n", cfg.Addr, cfg.DBName)
	fmt.Printf("集合前缀    : %s\n\n", prefix)

	cli, err := sdkConnect(ctx, cfg)
	if err != nil {
		fmt.Printf("连接 Milvus 失败: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = cli.Close(context.Background()) }()

	names, err := cli.ListCollections(ctx, milvusclient.NewListCollectionOption())
	if err != nil {
		fmt.Printf("列举集合失败: %v\n", err)
		os.Exit(1)
	}

	wanted := parseKBIDFilter(os.Getenv("TENANT_AUDIT_KBIDS"))
	var targets []string
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) {
			continue // 探测/诊断集合与非业务集合一律不碰
		}
		if wanted != nil {
			id, err := strconv.ParseInt(strings.TrimPrefix(n, prefix), 10, 64)
			if err != nil || !wanted[id] {
				continue
			}
		}
		targets = append(targets, n)
	}
	sort.Strings(targets)

	if len(targets) == 0 {
		fmt.Printf("没有找到前缀为 %s 的集合，无需回填。\n", prefix)
		return
	}

	rows := make([]row, 0, len(targets))
	var sumEmpty int
	var sumTotal int64
	cappedAny := false

	fmt.Printf("%-22s %12s %12s  %s\n", "集合", "总行数", "tenant空", "占比")
	fmt.Println(strings.Repeat("-", 72))

	for _, coll := range targets {
		r := row{coll: coll}

		// 总行数走集合统计，避免为了计数而把所有行拉回来
		if stats, serr := cli.GetCollectionStats(ctx, milvusclient.NewGetCollectionStatsOption(coll)); serr != nil {
			r.errText = "stats: " + shortErr(serr)
		} else {
			r.total = parseInt(stats["row_count"])
		}

		// 空 tenant 计数：只取 chunk_id 一列，输出字段尽量小
		rs, qerr := cli.Get(ctx, milvusclient.NewQueryOption(coll).
			WithFilter(fmt.Sprintf("%s == \"\"", fieldTenant)).
			WithOutputFields(fieldChunkID).
			WithLimit(queryHardLimit))
		if qerr != nil {
			// 未 load 的集合查询会失败，这不代表没有空值
			r.errText = strings.TrimSpace(r.errText + " query: " + shortErr(qerr))
		} else {
			r.empty = rs.Len()
			if r.empty >= queryHardLimit {
				r.capped = true
				cappedAny = true
			}
		}

		sumEmpty += r.empty
		sumTotal += r.total
		rows = append(rows, r)

		pct := "-"
		if r.total > 0 {
			pct = fmt.Sprintf("%.1f%%", float64(r.empty)/float64(r.total)*100)
		}
		mark := ""
		if r.capped {
			mark = fmt.Sprintf("  ⚠ 命中单次查询上限 %d，实际更多", queryHardLimit)
		}
		if r.errText != "" {
			mark += "  [!] " + r.errText
		}
		fmt.Printf("%-22s %12d %12s  %s%s\n", r.coll, r.total, countCell(r, mark), pct, mark)
	}

	fmt.Println(strings.Repeat("-", 72))
	fmt.Printf("集合数=%d  总行数≈%d  tenant 为空≥%d\n\n", len(rows), sumTotal, sumEmpty)

	if cappedAny {
		fmt.Println("注意：至少一个集合的空值计数达到单次查询上限，上面的总数是**下限**。")
		fmt.Println("      要精确数请对相关集合按 kb_id 分批跑（TENANT_AUDIT_KBIDS）。")
	}

	fmt.Println("判读方式：")
	fmt.Println("  · tenant 为空数 = 0        → 可把 buildFilter 收紧为等值过滤")
	fmt.Println("  · 少量且都是老数据         → 回填后收紧")
	fmt.Println("  · 量大或含无权访问历史风险  → 在回填完成前，检索隔离只依赖")
	fmt.Println("                              rag_service 的 authorizeKBIDs（入口校验），")
	fmt.Println("                              不要把这一层当成已生效的隔离")
	fmt.Println()
	fmt.Println("回填时每个 kb_id 应写入的 tenant 来自 PG（Milvus 里没有这个真相）：")
	fmt.Println("  SELECT kb_id, tenant, name FROM knowledge_bases ORDER BY kb_id;")
	fmt.Println("切片归属：")
	fmt.Println("  SELECT d.kb_id, COUNT(c.id) FROM documents d")
	fmt.Println("    JOIN chunks c ON c.doc_id = d.doc_id GROUP BY d.kb_id;")
}

func countCell(r row, mark string) string {
	// 查询失败时不能把 0 当成"没有空值"报出去 —— 那会被读成"安全"。
	if r.errText != "" && strings.Contains(r.errText, "query:") {
		return "查询失败"
	}
	if r.capped {
		return fmt.Sprintf("≥%d", r.empty)
	}
	return strconv.Itoa(r.empty)
}

func parseKBIDFilter(s string) map[int64]bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	m := map[int64]bool{}
	for _, part := range strings.Split(s, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
			m[id] = true
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func parseInt(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 90 {
		s = s[:90] + "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}

// sdkConnect 与 milvus_probe/milvus_diag 同一套两阶段连接。
func sdkConnect(ctx context.Context, cfg conf.MilvusConf) (*milvusclient.Client, error) {
	cli, err := milvusclient.New(ctx, &milvusclient.ClientConfig{
		Address:  cfg.Addr,
		Username: cfg.Username,
		Password: cfg.Password,
		// DBName 留空：建连时 SDK 会校验库是否存在，
		// 与适配层同样的理由（见 pkg/vectordb/milvus3/store.go 注释）。
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

// loadConf 与 milvus_probe.go 保持同样的环境变量优先级。
func loadConf() conf.MilvusConf {
	cfg := conf.MilvusConf{
		Addr:       envOr("MILVUS_ADDRESS", "127.0.0.1:19530"),
		Username:   os.Getenv("MILVUS_USERNAME"),
		Password:   os.Getenv("MILVUS_PASSWORD"),
		DBName:     envOr("MILVUS_DB", "aegis"),
		Dim:        1024,
		MetricType: "COSINE",
		Timeout:    30 * time.Second,
	}
	if v := os.Getenv("AEGIS_MILVUS__ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("AEGIS_MILVUS__DB_NAME"); v != "" {
		cfg.DBName = v
	}
	if v := os.Getenv("AEGIS_MILVUS__USERNAME"); v != "" {
		cfg.Username = v
	}
	if v := os.Getenv("AEGIS_MILVUS__PASSWORD"); v != "" {
		cfg.Password = v
	}
	return cfg
}
