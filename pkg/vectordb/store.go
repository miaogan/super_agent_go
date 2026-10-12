// Package vectordb 定义向量存储抽象。
//
// 分层原因（对应 ADR-006）：
//
//	Milvus 3.0.2 的 Go SDK（client/v3）与 eino-ext 的 milvus2 不兼容，
//	因此向量读写必须自研适配层。抽象出本接口后：
//	  - 业务代码（检索、入库）不依赖具体 Milvus 版本
//	  - mock 模式可用内存实现，冒烟测试无需 Milvus
//	  - 未来 Milvus 升级只改 vectordb/milvus3 一个包
package vectordb

import (
	"context"
	"errors"
)

// ErrUnavailable 向量库不可用（应触发降级）
//
// ★ 这是降级链的关键信号：检索层捕获此错误后
// 立即切到 PostgreSQL 关键词检索，而不是把 5xx 抛给用户。
var ErrUnavailable = errors.New("vectordb: 向量库不可用")

// ErrSearchFailed 检索执行失败
var ErrSearchFailed = errors.New("vectordb: 检索失败")

// ErrDimMismatch 向量维度不匹配
var ErrDimMismatch = errors.New("vectordb: 向量维度不匹配")

// SparseToken 稀疏向量的一维：{词表位置 → 权重}。
type SparseToken struct {
	Index int64
	Value float32
}

// Chunk 向量记录
type Chunk struct {
	// ChunkID 全局唯一切片 ID（PG 与 Milvus 共用同一标识）
	ChunkID string
	KBID    int64
	DocID   int64
	Seq     int32
	Content string
	// Vector 仅在写入与调试查询时填充，检索结果中为空（正文回 PG 取）
	Vector []float32
	// SparseVector 稀疏向量（词项权重），dense+sparse 混合检索用；可选
	SparseVector []SparseToken
	PageNo       int32
	// TokenCount 切片 token 数
	TokenCount int32
	// Tenant 用于检索时的租户隔离过滤
	Tenant string
}

// SearchRequest 检索请求
type SearchRequest struct {
	// Collection 目标集合（通常与 KB 一一对应，或统一集合 + kb_id 过滤）
	Collection string
	// Vector 查询向量（稠密）
	Vector []float32
	// SparseVector 查询稀疏向量（可选）。非空时走稀疏字段检索，
	// 与 Vector 互斥：一次 Search 只查一路，双路融合由上层负责。
	SparseVector []SparseToken
	// TopK 返回条数
	TopK int
	// ScoreThreshold 相似度阈值，低于此值的结果被丢弃
	ScoreThreshold float64
	// KBIDs 限定知识库范围
	KBIDs []int64
	// Tenant 租户隔离（必填时强制过滤）
	Tenant string
	// ExcludeDocIDs 排除的文档（如已被用户引用过的，避免重复召回）
	ExcludeDocIDs []int64
	// MetricType 相似度度量
	MetricType string
	// HNSWEf 检索深度
	HNSWEf int
}

// SearchResult 检索结果
type SearchResult struct {
	// ChunkID → Score
	Scores map[string]float64
	// 命中顺序（按分数降序）
	ChunkIDs []string
	// ElapsedMs 检索耗时
	ElapsedMs int64
}

// Store 向量存储接口
type Store interface {
	// EnsureCollection 确保集合存在且 schema 匹配。
	// 维度不一致必须报错而不是静默重建 —— 重建会丢数据。
	EnsureCollection(ctx context.Context, collection string, dim int) error

	// Upsert 写入/更新向量
	Upsert(ctx context.Context, collection string, chunks []*Chunk) error

	// DeleteByDoc 删除某文档的全部向量
	DeleteByDoc(ctx context.Context, collection string, docID int64) error

	// DeleteByKB 删除某知识库的全部向量
	DeleteByKB(ctx context.Context, collection string, kbID int64) error

	// Search 向量检索
	Search(ctx context.Context, req *SearchRequest) (*SearchResult, error)

	// QueryByIDs 按 ID 取向量（调试用）
	QueryByIDs(ctx context.Context, collection string, ids []string) ([]*Chunk, error)

	// Ping 健康检查
	Ping(ctx context.Context) error

	// Close 释放连接
	Close() error
}

// Syncer 写入后保证可见的**可选能力**。
//
// ---------- 为什么不把它放进 Store 主接口 ----------
//
//	「同步」这个需求来自 Milvus 的最终一致性模型，
//	不是所有向量库都有对应概念（也不都需要）。
//	放进主接口会迫使 mockstore 写一个空实现，
//	并让每个实现者都要回答「你这个后端需要同步吗」——
//	而真实答案往往是「不需要，写入即可见」。
//
//	拆成可选接口后：能力显式存在，用类型断言探测，
//	不存在的后端天然跳过，业务语义仍然完整。
//
// ---------- 为什么 Milvus 需要它（实测数据） ----------
//
//	Milvus 默认一致性等级是 ClBounded：
//	新写入的数据要等落盘后才保证对检索可见。
//	实测（Milvus 3.0.2，本项目真实实例）：
//
//	  Upsert → 立即 Search → 召回 0 条（无任何报错）
//	  Upsert → 等 1s → Search → 正常召回
//	  Upsert → Flush+Await → Search → 立即正常召回
//	  同一时刻 ClStrong 检索 → 正常召回
//
//	关键点：**它不报错**。表现是「刚上传的文档搜不到」，
//	而用户无从判断是系统坏了还是数据还没就绪。
//	在 RAG 场景下这是很差的体验 ——
//	文档刚上传成功、状态显示已就绪，紧接着提问却检索不到。
//
// ---------- Sync 的语义 ----------
//
//	「让此前所有写入对后续检索可见」，不是「清空缓存」，
//	也不是「保证永久存储」。实现方可以选择低成本近似
//	（比如仅推进一致性水位），只要调用方能信赖这个契约。
type Syncer interface {
	// Sync 保证此前对 collection 的写入在后续 Search 中可见。
	Sync(ctx context.Context, collection string) error
}

// SyncAfterWrite 在 store 支持时执行 Sync，否则什么都不做。
//
// ★ 为什么错误只被丢弃而不上抛：
//
//	写完但暂时不可见，是可用性问题而非数据丢失问题 ——
//	向量已经落在 Milvus 里，补偿任务照样能捞到，
//	数据不会丢。把 Sync 失败当成本次入库失败，
//	反而会把「已成功入库」误标为「待补偿」，
//	造成补偿任务反复重跑同一批数据。
//
//	调用方若需要感知，应在 Sync 后**读回验证**，
//	而不是把 Sync 的返回值当作入库成败的依据。
func SyncAfterWrite(ctx context.Context, s Store, collection string) error {
	if sy, ok := s.(Syncer); ok {
		return sy.Sync(ctx, collection)
	}
	return nil
}

// 支持Sync 的后端应把该方法实现为幂等的 ——
// 补偿任务可能对同一集合调用多次。

// Stats 存储统计
type Stats struct {
	Backend    string
	Collection string
	Dim        int
	Entities   int64
	Healthy    bool
}
