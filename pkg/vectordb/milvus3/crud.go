package milvus3

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"github.com/aegis/pkg/vectordb"
)

// Upsert 写入/更新向量。
//
// 用 Milvus 的 Upsert 而非 Insert：补偿任务会反复提交同一批 chunk，
// Insert 会为每次提交生成新行，导致同一 chunk_id 出现多条向量，
// 检索时靠 TopK 截断可能召回重复内容。
func (s *Store) Upsert(ctx context.Context, collection string, chunks []*vectordb.Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	dim := s.cfg.Dim
	ids := make([]string, 0, len(chunks))
	kbIDs := make([]int64, 0, len(chunks))
	docIDs := make([]int64, 0, len(chunks))
	seqs := make([]int64, 0, len(chunks))
	contents := make([]string, 0, len(chunks))
	pageNos := make([]int64, 0, len(chunks))
	tokenCounts := make([]int64, 0, len(chunks))
	tenants := make([]string, 0, len(chunks))
	vectors := make([][]float32, 0, len(chunks))
	sparseVecs := make([]entity.SparseEmbedding, 0, len(chunks))

	for _, c := range chunks {
		if c == nil || c.ChunkID == "" {
			// 跳过无效行而不是整批失败：
			// 一次脏数据不该让同批几百个正常 chunk 全部进不了库
			// （补偿任务会重试，失败原因在调用方已可见）。
			continue
		}
		if len(c.Vector) != dim {
			// ★ 这里必须报错而不是补零/截断：
			//   维度不符说明上游 embedding 模型换了或配置漂了，
			//   写入脏向量会让检索"能跑但结果全错"，事后极难排查。
			return fmt.Errorf("%w: chunk %s 向量维度 %d，配置维度 %d",
				vectordb.ErrDimMismatch, c.ChunkID, len(c.Vector), dim)
		}
		if len(c.Content) > contentFieldMaxLen {
			// 超长则拒绝而非截断：截断后的切片是半句话，
			// 检索出来会给出看似合理实则错误的答案。
			return fmt.Errorf("milvus3: chunk %s 正文 %d 字节，超过上限 %d"+
				"（请调小 rag.default_chunk_size）",
				c.ChunkID, len(c.Content), contentFieldMaxLen)
		}

		ids = append(ids, c.ChunkID)
		kbIDs = append(kbIDs, c.KBID)
		docIDs = append(docIDs, c.DocID)
		seqs = append(seqs, int64(c.Seq))
		contents = append(contents, c.Content)
		pageNos = append(pageNos, int64(c.PageNo))
		tokenCounts = append(tokenCounts, int64(c.TokenCount))
		tenants = append(tenants, c.Tenant)
		vectors = append(vectors, c.Vector)
		sparseVecs = append(sparseVecs, toEntitySparse(c.SparseVector))
	}
	if len(ids) == 0 {
		return nil
	}

	opt := milvusclient.NewColumnBasedInsertOption(collection).
		WithVarcharColumn(fieldChunkID, ids).
		WithInt64Column(fieldKBID, kbIDs).
		WithInt64Column(fieldDocID, docIDs).
		WithInt64Column(fieldSeq, seqs).
		WithVarcharColumn(fieldContent, contents).
		WithInt64Column(fieldPageNo, pageNos).
		WithInt64Column(fieldTokenCount, tokenCounts).
		WithVarcharColumn(fieldTenant, tenants).
		WithFloatVectorColumn(s.vectorField, dim, vectors).
		// 稀疏向量列没有专用 With 方法，用通用 WithColumns 追加
		WithColumns(column.NewColumnSparseVectors(fieldSparseVec, sparseVecs))

	if _, err := s.cli.Upsert(ctx, opt); err != nil {
		return classifyErr(err)
	}
	return nil
}

// DeleteByDoc 删除某文档的全部向量
func (s *Store) DeleteByDoc(ctx context.Context, collection string, docID int64) error {
	expr := fmt.Sprintf("%s == %d", fieldDocID, docID)
	return s.deleteBy(ctx, collection, expr)
}

// DeleteByKB 删除某知识库的全部向量
func (s *Store) DeleteByKB(ctx context.Context, collection string, kbID int64) error {
	expr := fmt.Sprintf("%s == %d", fieldKBID, kbID)
	return s.deleteBy(ctx, collection, expr)
}

func (s *Store) deleteBy(ctx context.Context, collection, expr string) error {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	// ★ 是 WithExpr 不是 WithFilter：
	//   searchOption/queryOption 用 WithFilter，
	//   而 deleteOption 在 v3 里叫 WithExpr —— 名字不统一，
	//   照记忆写会编译不过（这正是本文件第一次编译时的报错之一）。
	if _, err := s.cli.Delete(ctx, milvusclient.NewDeleteOption(collection).WithExpr(expr)); err != nil {
		return classifyErr(err)
	}
	return nil
}

// Search 向量检索。
//
// 支持两路（互斥，由 req 决定）：
//   - req.Vector 非空 → 稠密字段（vector）检索，度量 COSINE/L2/IP
//   - req.SparseVector 非空 → 稀疏字段（sparse_vector）检索，度量 IP
//
// ★ 一次只查一路：dense + sparse 的双路融合由上层（rag retrieve）负责，
//
//	这里保持"单次调用单一语义"，避免把融合策略耦合进存储层。
func (s *Store) Search(ctx context.Context, req *vectordb.SearchRequest) (*vectordb.SearchResult, error) {
	if req == nil || (len(req.Vector) == 0 && len(req.SparseVector) == 0) {
		return &vectordb.SearchResult{Scores: map[string]float64{}}, nil
	}

	topK := req.TopK
	if topK <= 0 {
		topK = 5
	}

	var queryVecs []entity.Vector
	if len(req.SparseVector) > 0 {
		// 稀疏检索：IP 内积（词项权重内积，见 createCollection 索引注释）
		sv, err := entity.NewSliceSparseEmbedding(
			sparsePositions(req.SparseVector), sparseValues(req.SparseVector))
		if err != nil {
			return nil, fmt.Errorf("milvus3: 构造稀疏查询向量失败: %w", err)
		}
		queryVecs = []entity.Vector{sv}
	} else {
		if len(req.Vector) != s.cfg.Dim {
			// 查询向量维度不符说明嵌入模型换了。
			// 直接返回不可用，让上层降级到关键词检索 ——
			// 总比拿维度错误的向量去检索得到一堆无意义结果好。
			return nil, fmt.Errorf("%w: 查询向量维度 %d，配置维度 %d",
				vectordb.ErrDimMismatch, len(req.Vector), s.cfg.Dim)
		}
		queryVecs = []entity.Vector{entity.FloatVector(req.Vector)}
	}

	opt := milvusclient.NewSearchOption(req.Collection, topK, queryVecs).
		WithOutputFields(fieldChunkID)

	if expr := buildFilter(req); expr != "" {
		opt = opt.WithFilter(expr)
	}
	// ef 只影响召回深度，不影响结果语义；配了才生效（仅稠密路径有意义）
	if req.HNSWEf > 0 && len(req.SparseVector) == 0 {
		opt = opt.WithSearchParam("ef", strconv.Itoa(req.HNSWEf))
	}

	// ★ 阈值过滤放在客户端做：
	//   不同度量（COSINE/L2/IP）的 score 含义完全不同，
	//   服务端的 range filter 表达不了跨度量的统一语义。
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	resultSets, err := s.cli.Search(ctx, opt)
	if err != nil {
		return nil, classifyErr(err)
	}

	out := &vectordb.SearchResult{
		Scores:   make(map[string]float64),
		ChunkIDs: make([]string, 0, topK),
	}
	if len(resultSets) == 0 {
		return out, nil
	}

	// 单查询向量 → 结果集下标 0 即对应本次查询
	rs := resultSets[0]
	if rs.Err != nil {
		return nil, classifyErr(rs.Err)
	}

	// 阈值过滤放在客户端做：
	// 不同度量（COSINE/L2/IP）的 score 含义完全不同，
	// 服务端的 range filter 表达不了跨度量的统一语义。
	for i := 0; i < rs.Len(); i++ {
		score := float64(rs.Scores[i])
		if req.ScoreThreshold > 0 && score < req.ScoreThreshold {
			// Milvus 已按分数降序返回，后面的只会更低，可以直接停
			break
		}
		id, ok := idAt(rs, i)
		if !ok {
			continue
		}
		out.ChunkIDs = append(out.ChunkIDs, id)
		out.Scores[id] = score
	}
	if len(out.ChunkIDs) == 0 {
		// ★ 显式返回空结果而非报错：
		//   「库里有数据但都不相关」是正常业务结论，不是故障。
		//   报错会让上层误判为检索失败并触发无谓的降级/告警。
		return &vectordb.SearchResult{Scores: map[string]float64{}}, nil
	}
	out.ElapsedMs = 0
	return out, nil
}

// idAt 取第 i 行的 chunk_id。
//
// ★ 为什么用 GetAsString 而不是类型断言成 *column.ColumnVarChar：
//
//	断言失败时只能返回零值，把「主键列类型不符」变成静默丢结果；
//	GetAsString 会把错误返回出来，且对 VarChar / JSON 等类型都适用。
func idAt(rs milvusclient.ResultSet, i int) (string, bool) {
	if i < 0 || i >= rs.Len() {
		return "", false
	}
	// 优先读主键列
	if rs.IDs != nil {
		if s, err := rs.IDs.GetAsString(i); err == nil && s != "" {
			return s, true
		}
	}
	// 兜底：读 output field 里的 chunk_id
	if col := rs.GetColumn(fieldChunkID); col != nil {
		if s, err := col.GetAsString(i); err == nil && s != "" {
			return s, true
		}
	}
	return "", false
}

// buildFilter 由检索请求构造 Milvus 过滤表达式。
//
// ★ Tenant 写成 `(tenant == "" or tenant == "x")` 的原因与遗留风险：
//
//	新 ingest 的切片已写入 Tenant（rag_service/internal/ingest/pipeline.go），
//	但改动之前入库的存量数据该字段是空串。若此刻严格按等值过滤，
//	这些切片会从检索结果里**静默消失** —— 不报错，只表现为"搜不到东西"。
//	所以保留 `== ""` 这一支只是为了不砸掉存量数据，它同时意味着：
//	在该 KB 的向量完成 tenant 回填之前，这一条不构成租户隔离。
//	真正的隔离目前依赖检索入口对 kb_ids 的归属校验
//	（rag_service/internal/service/rag.go 的 authorizeKBIDs）。
//	回填完成后应把这里收紧为 `tenant == "x"` 并删除 `== ""` 分支。
func buildFilter(req *vectordb.SearchRequest) string {
	var clauses []string

	if len(req.KBIDs) > 0 {
		parts := make([]string, 0, len(req.KBIDs))
		for _, id := range req.KBIDs {
			parts = append(parts, fmt.Sprintf("%s == %d", fieldKBID, id))
		}
		clauses = append(clauses, "("+strings.Join(parts, " or ")+")")
	}

	if req.Tenant != "" {
		clauses = append(clauses,
			fmt.Sprintf("(%s == \"\" or %s == %q)", fieldTenant, fieldTenant, req.Tenant))
	}

	if len(req.ExcludeDocIDs) > 0 {
		parts := make([]string, 0, len(req.ExcludeDocIDs))
		for _, id := range req.ExcludeDocIDs {
			parts = append(parts, fmt.Sprintf("%s != %d", fieldDocID, id))
		}
		clauses = append(clauses, "("+strings.Join(parts, " and ")+")")
	}

	return strings.Join(clauses, " and ")
}

// QueryByIDs 按 ID 取向量（调试用）。
func (s *Store) QueryByIDs(ctx context.Context, collection string, ids []string) ([]*vectordb.Chunk, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		// 逐个校验并转义：chunk_id 虽是我们自己生成的，
		// 但过滤表达式是字符串拼接，一个引号就能让表达式变形。
		parts = append(parts, fmt.Sprintf("%s == %s", fieldChunkID, quoteStr(id)))
	}
	expr := strings.Join(parts, " or ")

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	rs, err := s.cli.Get(ctx, milvusclient.NewQueryOption(collection).
		WithFilter(expr).
		WithOutputFields(
			fieldChunkID, fieldVector, fieldKBID, fieldDocID, fieldSeq,
			fieldContent, fieldPageNo, fieldTokenCount, fieldTenant))
	if err != nil {
		return nil, classifyErr(err)
	}
	if rs.Len() == 0 {
		return nil, nil
	}

	out := make([]*vectordb.Chunk, 0, rs.Len())
	var firstErr error // 只记第一条错误，避免日志被淹没
	for i := 0; i < rs.Len(); i++ {
		c := &vectordb.Chunk{}
		var ok bool
		if c.ChunkID, ok = strAt(rs, fieldChunkID, i); !ok {
			continue // chunk_id 取不到说明整行无效，跳过
		}
		// 其余字段取失败时记录，但保留已取到的零值继续 ——
		// 一行里某个标量字段缺失不应让整批结果全丢。
		// 调用方靠 chunk_id 做唯一标识，标量是辅助信息。
		if v, ok := strAt(rs, fieldContent, i); ok {
			c.Content = v
		} else if firstErr == nil {
			firstErr = fmt.Errorf("row %d: field %q missing or wrong type", i, fieldContent)
		}
		if v, ok := strAt(rs, fieldTenant, i); ok {
			c.Tenant = v
		} else if firstErr == nil {
			firstErr = fmt.Errorf("row %d: field %q missing or wrong type", i, fieldTenant)
		}
		if v, ok := intAt(rs, fieldKBID, i); ok {
			c.KBID = v
		} else if firstErr == nil {
			firstErr = fmt.Errorf("row %d: field %q missing or wrong type", i, fieldKBID)
		}
		if v, ok := intAt(rs, fieldDocID, i); ok {
			c.DocID = v
		} else if firstErr == nil {
			firstErr = fmt.Errorf("row %d: field %q missing or wrong type", i, fieldDocID)
		}
		if v, ok := int32At(rs, fieldSeq, i); ok {
			c.Seq = v
		} else if firstErr == nil {
			firstErr = fmt.Errorf("row %d: field %q missing or wrong type", i, fieldSeq)
		}
		if v, ok := int32At(rs, fieldPageNo, i); ok {
			c.PageNo = v
		} else if firstErr == nil {
			firstErr = fmt.Errorf("row %d: field %q missing or wrong type", i, fieldPageNo)
		}
		if v, ok := int32At(rs, fieldTokenCount, i); ok {
			c.TokenCount = v
		} else if firstErr == nil {
			firstErr = fmt.Errorf("row %d: field %q missing or wrong type", i, fieldTokenCount)
		}
		c.Vector = vecAt(rs, fieldVector, i)
		out = append(out, c)
	}
	if firstErr != nil && len(out) == 0 {
		// 全部行都有问题 → 返回 error 让上层感知
		return nil, fmt.Errorf("milvus QueryByIDs: 字段解析全部失败: %w", firstErr)
	}
	// 部分行有问题：返回部分结果 + nil error。
	// 调用方（rag_service 的 RecountKB / 补偿任务）宁可拿部分数据也不要空结果。
	return out, nil
}

// quoteStr 生成 Milvus 过滤表达式中的字符串字面量。
//
// Milvus 用双引号表示字符串，反斜杠与双引号都需转义。
// 未转义时，一个含引号的 chunk_id 就能让整个表达式变形，
// 轻则查不到数据，重则报语法错影响其他查询。
func quoteStr(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func strAt(rs milvusclient.ResultSet, field string, i int) (string, bool) {
	col := rs.GetColumn(field)
	if col == nil {
		return "", false
	}
	v, err := col.GetAsString(i)
	if err != nil {
		return "", false
	}
	return v, true
}

func intAt(rs milvusclient.ResultSet, field string, i int) (int64, bool) {
	col := rs.GetColumn(field)
	if col == nil {
		return 0, false
	}
	v, err := col.GetAsInt64(i)
	if err != nil {
		return 0, false
	}
	return v, true
}

func int32At(rs milvusclient.ResultSet, field string, i int) (int32, bool) {
	v, ok := intAt(rs, field, i)
	return int32(v), ok
}

// vecAt 取第 i 行的向量。
//
// ★ 为什么必须做类型断言而不能用泛型取值接口：
//
//	column.Column 接口只暴露 GetAsInt64 / GetAsString /
//	GetAsDouble / GetAsBool 这些标量取值方法，
//	向量列（ColumnFloatVector）不在其中——
//	它是泛型容器 vectorBase[[]float32]，
//	只能断言到具体类型后调 Value(i)。
func vecAt(rs milvusclient.ResultSet, field string, i int) []float32 {
	col := rs.GetColumn(field)
	if col == nil {
		return nil
	}
	fv, ok := col.(*column.ColumnFloatVector)
	if !ok {
		return nil
	}
	v, err := fv.Value(i)
	if err != nil {
		return nil
	}
	return v
}

// toEntitySparse 把 vectordb.SparseToken 转成 Milvus 的 SparseEmbedding。
// 空输入返回 nil（Milvus 稀疏字段允许空，表示该行无稀疏向量）。
func toEntitySparse(toks []vectordb.SparseToken) entity.SparseEmbedding {
	positions := []uint32{}
	values := []float32{}
	if len(toks) > 0 {
		positions = sparsePositions(toks)
		values = sparseValues(toks)
	}
	// ★ 始终返回合法 SparseEmbedding 实例，绝不能返回 nil interface：
	//   Milvus client v3 的 ColumnSparseFloatVector.FieldData() 拿到 nil
	//   会直接 panic（nil pointer dereference）。空稀疏向量用空 slice 构造。
	sv, _ := entity.NewSliceSparseEmbedding(positions, values)
	return sv
}

func sparsePositions(toks []vectordb.SparseToken) []uint32 {
	out := make([]uint32, len(toks))
	for i, t := range toks {
		out[i] = uint32(t.Index)
	}
	return out
}

func sparseValues(toks []vectordb.SparseToken) []float32 {
	out := make([]float32, len(toks))
	for i, t := range toks {
		out[i] = t.Value
	}
	return out
}
