// Package milvus3 是 Milvus 3.x 的vectordb.Store 实现。
//
// ---------- 为什么必须自研（ADR-006） ----------
//
//	eino-ext 官方只提供 milvus2（依赖 client/v2），
//	与Milvus 3.0 的 client/v3 **类型不互通**：
//	  - proto 包不同：milvus-proto/go-api/v2 → v3
//	  - schema 类型不同：v2 的 entity.FieldSchema → v3 的 entity.Field
//	  - 客户端形态不同：v2 的 Client 是 interface，v3 是具体 struct
//	所以无法直接复用，只能按接口面自研。
//
// ---------- 与 mockstore 的行为差异（必须留意） ----------
//
//  1. 持久性：本实现数据落 Milvus，进程重启不丢；mockstore 重启即丢。
//  2. 索引：HNSW 近似检索，耗时随数据量亚线性增长；
//     mockstore 是全量暴力比对，随数据量线性增长。
//  3. 过滤时机：Milvus 在服务端过滤（filter 表达式下推），
//     mockstore 在内存遍历后过滤 —— 结果语义一致，耗时特征不同。
//
// ---------- 租户隔离的一个现实问题 ----------
//
// vectordb.Chunk 有 Tenant 字段，SearchRequest 也有，但
// **ingest 流水线目前不写 Tenant**（见 rag_service/internal/ingest/pipeline.go）。
// mockstore 的判断是 `c.Tenant != "" && req.Tenant != ""`，
// 两边都空则不过滤，因此 mock 模式一直没暴露这个问题。
//
// Milvus 侧不能这样：如果直接按 tenant == "xxx" 过滤，
// 而存量数据的 tenant 字段为空字符串，**检索会全部落空**。
// 故本实现的选择是：
//   - 写入时把 Tenant 落库（不丢信息，为将来启用预留）
//   - 过滤时用 `tenant == "" or tenant == "x"` 兼容历史空值
//     等pipeline 补上 Tenant 写入后，可收紧为严格等值。
package milvus3

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/index"
	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/vectordb"
)

// 字段名常量。
//
// ★ 为什么不把字段名做成可配置：
//
//	schema 一旦创建就不能改字段名（Milvus 不支持 RenameField），
//	所以字段名是**写进存量数据**的契约，不能随配置漂移。
//	真要改名只能重建集合 + 全量重新向量化。
const (
	fieldChunkID    = "chunk_id"
	fieldVector     = "vector"
	fieldSparseVec  = "sparse_vector" // 稀疏向量（词项权重），dense+sparse 混合检索
	fieldKBID       = "kb_id"
	fieldDocID      = "doc_id"
	fieldSeq        = "seq"
	fieldContent    = "content"
	fieldPageNo     = "page_no"
	fieldTokenCount = "token_count"
	fieldTenant     = "tenant"
)

// contentFieldMaxLen content 字段的 max_length。
//
// ★ 为什么是 65535 而不是更小：
//
//	chunk 文本来自文档切片，默认 chunk_size=800 token，
//	实测中文切片通常 1000–3000 字符，65535 已有20 倍余量。
//	但也不能设成 65535*1024 —— VarChar 的 max_length 直接影响
//	内存分配，且 Milvus 单值上限就是 65535。
//	定这个值的代价：超长切片会被截断（见 Upsert 中的显式检查），
//	宁可入库时报错，也不要静默截断后检索出半句话。
const contentFieldMaxLen = 65535

// metricByName 配置里的度量名 → entity.MetricType
func metricByName(name string) entity.MetricType {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "L2":
		return entity.L2
	case "IP":
		return entity.IP
	default:
		// COSINE：归一化后内积 = 余弦，与 mockstore 的 Cosine 一致
		return entity.COSINE
	}
}

// Store Milvus 3.x 实现
type Store struct {
	cli *milvusclient.Client
	cfg conf.MilvusConf
	// vectorField 向量字段名。固定为 "vector" 而非取配置首字段：
	// 存量集合的向量字段名不可变更，配置化只会制造不一致。
	vectorField string
}

// New 连接 Milvus 并返回 Store。
//
// ★ 为什么连接失败不返回 ErrUnavailable：
//
//	构造期失败属于「配置/部署错误」，必须让服务**启动失败** ——
//	静默降级会让一个连错地址的部署看起来一切正常，
//	直到某天检索悄悄返回空结果才发现。
//	运行期的连接失效才由各方法转成 ErrUnavailable 触发降级。
func New(ctx context.Context, cfg conf.MilvusConf) (*Store, error) {
	if cfg.Addr == "" {
		return nil, errors.New("milvus3: addr 为空")
	}
	if cfg.Dim <= 0 {
		return nil, fmt.Errorf("milvus3: dim 非法（%d），必须为正数", cfg.Dim)
	}

	// ---------- 两阶段连接：先连默认库，建库后再切 ----------
	//
	// ★ 为什么不能把 DBName 直接塞进 ClientConfig：
	//
	//	SDK 在 New() 内部 connect() 时会做一次数据库校验，
	//	库不存在就直接返回 `database not found[database=xxx]`——
	//	连接阶段就失败，后面的建库代码根本没机会执行。
	//
	//	实测（Milvus 3.0.2 全新实例）：
	//	  New(DBName="aegis") → database not found[database=aegis]
	//	  New(DBName="")      → 连接成功 → CreateDatabase → UseDatabase
	//
	//	顺序必须是：连默认库 → 确保库存在 → 切库。
	//	对已存在的库同样无害（ListDatabase 命中后不重复建）。
	clientCfg := &milvusclient.ClientConfig{
		Address:  cfg.Addr,
		Username: cfg.Username,
		Password: cfg.Password,
		// DBName 刻意留空，理由见上。
	}
	// 超时：不设 DialOptions，走 SDK 默认的建连策略。
	//
	// ★ 已核实 SDK 实现（milvusclient.New → c.connect）：
	//   New() 里**确实会建立连接**，不是懒连接，
	//   所以 ctx 的超时/取消在这里是有效的 ——
	//   调用方应给建连阶段一个明确的 ctx 截止时间，
	//   否则地址不可达时启动流程会挂在SDK 的重试链上。
	//
	//   不额外塞 DialOptions 的原因：SDK 的 dialOptions 里
	//   已经配好传输凭据与重试拦截器，自定义 DialOptions 容易
	//   覆盖掉这些必要设置，得不偿失。超时交给 ctx 即可。
	cli, err := milvusclient.New(ctx, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("milvus3: 连接 %s 失败: %w", cfg.Addr, err)
	}

	// 确保数据库存在并切过去。
	//
	// 「库不存在」是全新部署的正常初始状态，不是配置错误 ——
	// 所以这里自动创建，而不是让用户先手动建库再启服务。
	if cfg.DBName != "" {
		if err := ensureDatabase(ctx, cli, cfg.DBName); err != nil {
			_ = cli.Close(context.Background())
			return nil, err
		}
	}

	return &Store{cli: cli, cfg: cfg, vectorField: fieldVector}, nil
}

// ensureDatabase 数据库不存在则创建，然后切换过去。
func ensureDatabase(ctx context.Context, cli *milvusclient.Client, dbName string) error {
	dbs, err := cli.ListDatabase(ctx, milvusclient.NewListDatabaseOption())
	if err != nil {
		return fmt.Errorf("milvus3: 列出数据库失败: %w", classifyErr(err))
	}
	found := false
	for _, db := range dbs {
		if db == dbName {
			found = true
			break
		}
	}
	if !found {
		if err := cli.CreateDatabase(ctx, milvusclient.NewCreateDatabaseOption(dbName)); err != nil {
			// 并发创建导致的「已存在」不算失败：
			// 多个 rag_service 实例同时启动时会同时走到这里。
			if strings.Contains(strings.ToLower(err.Error()), "already exist") {
				found = true
			} else {
				return fmt.Errorf("milvus3: 创建数据库 %s 失败: %w", dbName, classifyErr(err))
			}
		}
	}
	// ★ 必须显式切库：ClientConfig 里刻意没传 DBName，
	//   所以连接停在默认库上，不切的话后续所有操作都落在错的库里。
	if err := cli.UseDatabase(ctx, milvusclient.NewUseDatabaseOption(dbName)); err != nil {
		return fmt.Errorf("milvus3: 切换到数据库 %s 失败: %w", dbName, classifyErr(err))
	}
	return nil
}

var _ vectordb.Store = (*Store)(nil)

// withTimeout 给操作加上配置的超时上限。
//
// ★ 为什么需要它：
//
//	grpc 的 ctx 取消只保证「不再等待」，但 Milvus 侧的 query node
//	仍可能继续执行一段时间。若不设上限，一个慢查询会一直占着
//	goroutine 与连接，在批量入库/多知识库并发检索时迅速耗尽。
//
//	注意：超时返回的是 ctx.Err()，会被 classifyErr 归到
//	ErrUnavailable —— 这是**有意为之**：超时意味着这次检索拿不到结果，
//	上层降级到关键词检索比让用户干等更合适。
func (s *Store) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.cfg.Timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.cfg.Timeout)
}

// classifyErr 把 SDK 错误翻译成 vectordb 的标准错误。
//
// ★ 为什么要区分「不可用」与「检索失败」：
//
//	vectordb.ErrUnavailable 会触发上层切到关键词检索（降级链生效），
//	而其他错误只会被当作本次请求失败。
//	把所有错误都报成 Unavailable 等于「任何 Milvus 问题都静默降级」，
//	会掩盖真正的故障（比如维度不匹配、认证失败）。
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unavailable"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "context deadline"),
		strings.Contains(msg, "no connection"),
		strings.Contains(msg, "transport is closing"),
		strings.Contains(msg, "connection reset"):
		return fmt.Errorf("%w: %v", vectordb.ErrUnavailable, err)
	case strings.Contains(msg, "dim mismatch"),
		strings.Contains(msg, "dimension"),
		strings.Contains(msg, "not equal to"):
		return fmt.Errorf("%w: %v", vectordb.ErrDimMismatch, err)
	default:
		return fmt.Errorf("%w: %v", vectordb.ErrSearchFailed, err)
	}
}

// EnsureCollection 确保集合存在且维度一致。
//
// ★ 维度不一致必须报错而非重建：
//
//	重建集合会丢弃全部已入库向量，而 PG 里chunks 表还在——
//	结果是「向量库空了但元数据说已就绪」，比直接报错难查得多。
func (s *Store) EnsureCollection(ctx context.Context, collection string, dim int) error {
	if dim <= 0 {
		return fmt.Errorf("milvus3: 维度非法 %d", dim)
	}
	has, err := s.cli.HasCollection(ctx, milvusclient.NewHasCollectionOption(collection))
	if err != nil {
		return classifyErr(err)
	}
	if !has {
		return s.createCollection(ctx, collection, dim)
	}

	desc, err := s.cli.DescribeCollection(ctx,
		milvusclient.NewDescribeCollectionOption(collection))
	if err != nil {
		return classifyErr(err)
	}
	if got := vectorDimOf(desc, s.vectorField); got != 0 && got != dim {
		return fmt.Errorf("%w: 集合 %s 实际维度 %d，期望 %d"+
			"（重建集合会丢失已入库向量，请确认配置变更后执行全量重新向量化）",
			vectordb.ErrDimMismatch, collection, got, dim)
	}
	// 已存在的集合同样要确认加载态：
	// Milvus 重启后集合会回到 NotLoad 状态，
	// 而检索一个未加载的集合报的是 `collection not loaded`——
	// 那个错误信息完全看不出「其实只是没加载」。
	return s.ensureLoaded(ctx, collection)
}

// ensureLoaded 确保集合已加载到 query node。
//
// ★ 为什么这是必需的而不是可选优化：
//
//	Milvus 的检索/查询走 query node，集合必须先load 进内存。
//	新建集合默认不加载，直接检索会报
//	  `collection not loaded[collection=xxx]`
//	而该错误信息里没有任何「请先 load」的提示，
//	第一次接入很容易误判成集合没建成功或 schema 有问题。
//
//	为什么不能靠「建完就等一会」：
//	load 是异步任务（LoadCollection 返回 LoadTask），
//	必须 Await 到真正 Loaded 再继续，否则是竞态。
func (s *Store) ensureLoaded(ctx context.Context, collection string) error {
	state, err := s.cli.GetLoadState(ctx,
		milvusclient.NewGetLoadStateOption(collection))
	if err != nil {
		return classifyErr(err)
	}
	if state.State == entity.LoadStateLoaded {
		return nil
	}

	task, err := s.cli.LoadCollection(ctx,
		milvusclient.NewLoadCollectionOption(collection))
	if err != nil {
		return classifyErr(err)
	}
	// Await 会按interval 轮询直到 Loaded；
	// 若ctx 先超时则返回错误 —— 此时集合可能只是加载慢，
	// 上层会归为 ErrUnavailable 并降级，不会误判为数据问题。
	if err := task.Await(ctx); err != nil {
		return fmt.Errorf("milvus3: 等待集合 %s 加载完成超时或失败: %w", collection, err)
	}
	return nil
}

// vectorDimOf 从 schema 中取向量字段维度，取不到返回 0
func vectorDimOf(desc *entity.Collection, field string) int {
	if desc == nil || desc.Schema == nil {
		return 0
	}
	for _, f := range desc.Schema.Fields {
		if f.Name != field {
			continue
		}
		if d, err := strconv.Atoi(f.TypeParams["dim"]); err == nil {
			return d
		}
	}
	return 0
}

func (s *Store) createCollection(ctx context.Context, collection string, dim int) error {
	schema := entity.NewSchema().
		WithName(collection).
		WithDynamicFieldEnabled(false).
		// 主键用chunk_id（全局唯一），不用自增 ID：
		// 补偿任务会反复 Upsert 同一批 chunk，自增 ID 会产生重复行。
		WithField(entity.NewField().
			WithName(fieldChunkID).WithDataType(entity.FieldTypeVarChar).
			WithMaxLength(512).WithIsPrimaryKey(true)).
		WithField(entity.NewField().
			WithName(fieldVector).WithDataType(entity.FieldTypeFloatVector).
			WithDim(int64(dim))).
		WithField(entity.NewField().
			WithName(fieldSparseVec).WithDataType(entity.FieldTypeSparseVector)).
		WithField(entity.NewField().
			WithName(fieldKBID).WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().
			WithName(fieldDocID).WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().
			WithName(fieldSeq).WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().
			WithName(fieldContent).WithDataType(entity.FieldTypeVarChar).
			WithMaxLength(contentFieldMaxLen)).
		WithField(entity.NewField().
			WithName(fieldPageNo).WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().
			WithName(fieldTokenCount).WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().
			WithName(fieldTenant).WithDataType(entity.FieldTypeVarChar).
			WithMaxLength(256))

	// 索引类型选择：
	//   配了 hnsw_m就用 HNSW（参数可控），
	//   否则用 AutoIndex —— 由 Milvus 按数据规模自动选型，
	//   免去我们为「还不知道多大的数据量」手工猜索引参数。
	//
	// ★ 为什么 HNSW 参数是建索引选项而不是 CreateCollection 的选项：
	//   v3 的 createCollectionOption 没有 WithHNSWParam，
	//   HNSW 的 M / efConstruction 随索引定义一起提交
	//   （见 index.NewHNSWIndex）。
	var vecIdx index.Index
	if s.cfg.HNSWM > 0 {
		vecIdx = index.NewHNSWIndex(metricByName(s.cfg.MetricType),
			s.cfg.HNSWM, s.cfg.HNSWEfConstruction)
	} else {
		vecIdx = index.NewAutoIndex(metricByName(s.cfg.MetricType))
	}

	// 标量索引用在 kb_id / doc_id 上：检索必带 kb_id 过滤，
	// 删除则按 doc_id 过滤，二者都需要服务端裁剪而不是全量扫描。
	// 稀疏向量用 SPARSE_INVERTED_INDEX + IP（内积）：稀疏检索的
	// 相似度语义是词项权重内积，与稠密的 COSINE 度量不同。
	idxOpts := []milvusclient.CreateIndexOption{
		milvusclient.NewCreateIndexOption(collection, fieldVector, vecIdx).
			WithIndexName("idx_vector"),
		milvusclient.NewCreateIndexOption(collection, fieldSparseVec,
			index.NewSparseInvertedIndex(entity.IP, 0.2)).
			WithIndexName("idx_sparse_vector"),
		milvusclient.NewCreateIndexOption(collection, fieldKBID,
			index.NewSortedIndex()).WithIndexName("idx_kb_id"),
		milvusclient.NewCreateIndexOption(collection, fieldDocID,
			index.NewSortedIndex()).WithIndexName("idx_doc_id"),
	}

	opt := milvusclient.NewCreateCollectionOption(collection, schema).
		WithIndexOptions(idxOpts...)
	if err := s.cli.CreateCollection(ctx, opt); err != nil {
		return classifyErr(err)
	}
	// 新建集合后必须加载，否则后续检索报 collection not loaded。
	return s.ensureLoaded(ctx, collection)
}
