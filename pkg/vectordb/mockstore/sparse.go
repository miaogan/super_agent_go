package mockstore

import "github.com/aegis/pkg/vectordb"

// sparseScore 稀疏向量相似度：词项权重内积（与 Milvus 稀疏检索 IP 度量一致）。
//
// 实现为双指针归并（两向量均按位置升序），O(n+m)。
// 零重叠返回 0 —— 与查询无共享词的切片不得分，保证术语检索语义。
func sparseScore(query, doc []vectordb.SparseToken) float64 {
	if len(query) == 0 || len(doc) == 0 {
		return 0
	}
	var dot float64
	i, j := 0, 0
	for i < len(query) && j < len(doc) {
		switch {
		case query[i].Index < doc[j].Index:
			i++
		case query[i].Index > doc[j].Index:
			j++
		default:
			dot += float64(query[i].Value) * float64(doc[j].Value)
			i++
			j++
		}
	}
	return dot
}
