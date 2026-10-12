//go:build ignore

// 清理探测/诊断脚本在 Milvus 里留下的临时集合。
//
// ★ 为什么需要它：
//
//	milvus_probe.go 刻意**不删集合** ——
//	探测时反复建删集合，万一集合名被指到业务集合，
//	「删集合」这个动作本身就可能造成真实数据丢失。
//	代价就是每次探测/诊断都会留下一批 aegis_probe_* / aegis_diag_* 集合，
//	跑多了会堆积（本项目实测跑几轮就攒了 5 个）。
//
//	本脚本负责收拾，且采用**白名单前缀**策略：
//	只删名字以 aegis_probe / aegis_diag 开头的集合，
//	业务集合（aegis_kb_*）一个都不碰。
//
// 用法：
//
//	source scripts/goenv.sh
//	set -a && source .env && set +a
//	go run scripts/drop_probe_colls.go
//
// 可选环境变量：
//
//	MILVUS_DROP_DRY_RUN=1   只列出将删除哪些，不实际删除
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/milvus-io/milvus/client/v3/milvusclient"
)

// probePrefixes 允许删除的集合名前缀。
//
// ★ 这里用**白名单**而不是「列出不要删的」，方向很重要：
//
//	黑名单在新增命名规则时会漏掉，
//	漏掉的后果是**误删业务数据**。
//	白名单在新增命名规则时只会不删，
//	不删的后果仅仅是留下垃圾 —— 两边的错误代价完全不对称。
var probePrefixes = []string{"aegis_probe", "aegis_diag"}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	addr := rewriteHost(envOr("MILVUS_ADDRESS", "127.0.0.1:19530"))
	db := envOr("MILVUS_DB", "aegis")
	dryRun := os.Getenv("MILVUS_DROP_DRY_RUN") == "1"

	cli, err := milvusclient.New(ctx, &milvusclient.ClientConfig{
		Address:  addr,
		Username: os.Getenv("MILVUS_USERNAME"),
		Password: os.Getenv("MILVUS_PASSWORD"),
		// DBName 留空：SDK 建连时会校验库是否存在，
		// 而 aegis 库未必已建（见 milvus3 适配层的两阶段连接）。
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 连接 Milvus 失败: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = cli.Close(context.Background()) }()

	if db != "" {
		if err := cli.UseDatabase(ctx, milvusclient.NewUseDatabaseOption(db)); err != nil {
			fmt.Fprintf(os.Stderr, "✗ 切换数据库 %s 失败: %v\n", db, err)
			os.Exit(1)
		}
	}

	cols, err := cli.ListCollections(ctx, milvusclient.NewListCollectionOption())
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 列出集合失败: %v\n", err)
		os.Exit(1)
	}
	if len(cols) == 0 {
		fmt.Println("库里没有集合，无需清理")
		return
	}
	fmt.Printf("数据库 %s 现有集合 %d 个: %v\n\n", db, len(cols), cols)

	deleted := 0
	for _, c := range cols {
		if !isProbeCollection(c) {
			fmt.Printf("  跳过 %s（不在白名单前缀内）\n", c)
			continue
		}
		if dryRun {
			fmt.Printf("  [dry-run] 将删除 %s\n", c)
			deleted++
			continue
		}
		if err := cli.DropCollection(ctx, milvusclient.NewDropCollectionOption(c)); err != nil {
			fmt.Printf("  ✗ 删除 %s 失败: %v\n", c, err)
			continue
		}
		fmt.Printf("  ✓ 已删除 %s\n", c)
		deleted++
	}

	suffix := "已删除"
	if dryRun {
		suffix = "（dry-run，未实际删除）"
	}
	fmt.Printf("\n处理完成：%d 个探测集合%s\n", deleted, suffix)
	if left, err := cli.ListCollections(ctx, milvusclient.NewListCollectionOption()); err == nil {
		fmt.Printf("当前剩余集合: %v\n", left)
	}
}

// isProbeCollection 判断集合是否为探测/诊断产生。
func isProbeCollection(name string) bool {
	for _, p := range probePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// rewriteHost 把容器内主机名改写成宿主可达地址。
//
// .env 里为了服务容器间通信写的是 milvus:19530，
// 本脚本跑在宿主机上 —— 直接用它会报 context deadline exceeded，
// 看着像「Milvus 没起来」，实际只是宿主机 DNS 解析不了。
func rewriteHost(addr string) string {
	for _, host := range []string{"milvus", "aegis-milvus"} {
		if strings.HasPrefix(addr, host+":") {
			return "127.0.0.1:" + strings.TrimPrefix(addr, host+":")
		}
	}
	return addr
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
