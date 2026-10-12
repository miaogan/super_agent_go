package milvus3

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"github.com/aegis/pkg/vectordb"
)

// 编译期断言：milvus3 必须实现 vectordb.Syncer。
//
// ★ 为什么单独断言而不是靠调用处的类型断言：
//
//	类型断言失败会被静默跳过 —— 表现是
//	「代码写了 Sync，但库里读不到刚写的数据」，
//	而且没有任何报错。这种「安静地不生效」最难查，
//	所以在编译期就把它钉死。
var _ vectordb.Syncer = (*Store)(nil)

// Ping 健康检查。
//
// ★ 为什么要探活而不是直接返回 nil：
//
//	这个方法被降级探针调用（见 rag_service 的健康聚合）。
//	返回 nil 等于告诉探针「向量库可用」，而实际已经连不上——
//	那会让整套降级机制在最需要它的时候失效。
//	这里做一次轻量的 ListCollections，比连一个固定集合更稳：
//	集合被误删时它仍然能通，不至于把「集合缺失」误报成「服务不可用」。
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.cli == nil {
		return vectordb.ErrUnavailable
	}
	if _, err := s.cli.ListCollections(ctx, milvusclient.NewListCollectionOption()); err != nil {
		return classifyErr(err)
	}
	return nil
}

// Close 释放连接。
func (s *Store) Close() error {
	if s == nil || s.cli == nil {
		return nil
	}
	s.cli.Close(context.Background())
	return nil
}

// ---------- Sync：让已写入的数据立即可检索 ----------

// syncTimeoutFlush 单次 Flush 的等待上限。
//
// 实测Flush+Await 通常 1.0–1.3s（SDK 默认 200ms 轮询间隔，
// 一般 5–6 轮返回）。留30s 余量以应对 collection 较大时的落盘抖动，
// 但必须有上限：Sync 失败不能把入库流程挂死。
const syncTimeoutFlush = 30 * time.Second

// Sync 实现 vectordb.Syncer：Flush 集合并等待落盘完成。
//
// ---------- 为什么需要这个方法（实测结论，不是推测） ----------
//
//	Milvus 默认一致性等级 ClBounded，新写入数据在落盘前对检索不可见。
//	在本项目真实 Milvus 3.0.2 上实测：
//
//	  Upsert → 立即 Search      → 召回 0 条，**且不报任何错**
//	  Upsert → 等 1s → Search   → 正常召回
//	  Upsert → Flush → Search   → 立即正常召回
//
//	「不报错」这一点最要命：用户上传文档后立刻提问，
//	系统会告诉他「没找到相关内容」，而不是「数据还在就绪中」。
//	而文档状态此时已经是「已就绪」—— 状态与实际行为直接矛盾。
//
// ---------- 为什么不在 Search 里改用 ClStrong ----------
//
//	实测 ClStrong 确实能立刻看到新数据（E6 验证通过），
//	但那是拿**每次检索的延迟**换一致性：
//	Strong 需要等待所有写入落盘，检索是高频操作，
//	在 QPS 上来了之后这个代价会放大成用户可感知的卡顿。
//
//	而 Sync 只在**写入侧**付出一次成本：
//	入库是低频批量操作，1.2s 相对整个切片+向量化耗时可忽略。
//
//	**在低频写路径付一次钱，好过在高频读路径反复付。**
//
// ---------- 为什么不能无条件在每批 Upsert 后 Sync ----------
//
//	入库是分批的（默认 32 条/批）。一个 100 页文档可能切出
//	几百个 chunk，若每批都 Flush，就是几百次 × 1.2s ——
//	完全不可接受。
//
//	所以 Sync 由**调用方在整篇文档写完后显式调用一次**
//	（见 rag_service/internal/ingest/pipeline.go 的 vectorize 末尾）。
//	这样「一篇文档 = 一次 Sync」，代价恒定。
func (s *Store) Sync(ctx context.Context, collection string) error {
	if s == nil || s.cli == nil {
		return vectordb.ErrUnavailable
	}

	// ★ 用独立的 flushTimeout 而不是 s.cfg.Timeout：
	//   cfg.Timeout 是给单次 RPC 的上限（默认 30s），
	//   而 Flush 的 Await 是「提交 + 轮询直到落盘」，
	//   语义上更接近一个后台任务，超时给太小会在
	//   集合较大时误报失败。
	fctx, cancel := context.WithTimeout(ctx, syncTimeoutFlush)
	defer cancel()

	task, err := s.cli.Flush(fctx, milvusclient.NewFlushOption(collection))
	if err != nil {
		return classifyErr(err)
	}
	// 必须 Await：Flush RPC 是**异步提交**，
	// 立即返回只代表「服务端收到了 flush 请求」，
	// 数据此刻可能还没落盘 —— 那就等于什么都没做。
	if err := task.Await(fctx); err != nil {
		return fmt.Errorf("milvus3: 等待集合 %s flush 完成超时或失败: %w", collection, err)
	}
	return nil
}

// Stats 集合统计。
//
// ★ 关于 Entities 可能为 0：
//
//	Milvus 的 row_count 来自已落盘的 segment，
//	刚写入的数据在 flush / compaction 之前不计入统计。
//	因此「写入后立刻查Stats 得到 0」是**正常现象**，
//	不是写入失败的信号 —— 判断写入是否成功应该看 Search 能否召回。
//	这一点在此显式说明，避免看监控的人把 0 当成故障。
func (s *Store) Stats(ctx context.Context, collection string) (*vectordb.Stats, error) {
	if s == nil || s.cli == nil {
		return nil, vectordb.ErrUnavailable
	}
	raw, err := s.cli.GetCollectionStats(ctx,
		milvusclient.NewGetCollectionStatsOption(collection))
	if err != nil {
		return nil, classifyErr(err)
	}

	// Milvus 返回形如 {"row_count": "1024"} 的字符串键值对。
	//键名已实测确认为 row_count（不是 rowCount / entity_count）。
	var entities int64
	if v, ok := raw["row_count"]; ok {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
			entities = n
		}
	}

	dim := s.cfg.Dim
	healthy := true
	if err := s.Ping(ctx); err != nil {
		healthy = false
		if !errors.Is(err, vectordb.ErrUnavailable) {
			return nil, err
		}
	}
	return &vectordb.Stats{
		Backend:    "milvus3",
		Collection: collection,
		Dim:        dim,
		Entities:   entities,
		Healthy:    healthy,
	}, nil
}
