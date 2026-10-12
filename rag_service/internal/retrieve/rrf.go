package retrieve

import (
	"sort"
)

// ---------- RRF（Reciprocal Rank Fusion）融合 ----------

// rrfItem 融合后的一个条目。
type rrfItem struct {
	// id 条目唯一标识：KB 切片用 chunk_id，web 结果用合成 "web:"+hash
	id string
	// score 融合后的 RRF 分
	score float64
	// perSource 该条目在各来源的排名（sourceIndex → rank，1 起）
	perSource map[int]int
}

// fuseRRF 对多来源召回结果做倒数排名融合。
//
// 算法：每条目 score = Σ 1/(k + rank_i)，k 默认 60（经典取值）。
// 同一 id 在多个来源出现时累加，只在单来源出现的条目也能获分
// （这正是 RRF 优于简单并集去重之处：稀有来源的独特命中不丢）。
//
// 返回按融合分降序的条目列表（不含原始分数，调用方另行回填）。
func fuseRRF(sources [][]string, k int) []rrfItem {
	if k <= 0 {
		k = 60
	}
	score := make(map[string]float64)
	perSource := make(map[string]map[int]int)

	for si, ids := range sources {
		for rank, id := range ids {
			if id == "" {
				continue
			}
			score[id] += 1 / float64(k+rank+1)
			if perSource[id] == nil {
				perSource[id] = make(map[int]int)
			}
			// 同名条目在单来源内不应重复（调用方保证去重）；
			// 防御性处理：保留更优名次
			if r, ok := perSource[id][si]; !ok || rank+1 < r {
				perSource[id][si] = rank + 1
			}
		}
	}

	items := make([]rrfItem, 0, len(score))
	for id, s := range score {
		items = append(items, rrfItem{id: id, score: s, perSource: perSource[id]})
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].score > items[j].score
	})
	return items
}

// fuseIDs 只取融合后的有序 id 列表。
func fuseIDs(items []rrfItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.id)
	}
	return out
}

// rankOf 返回条目在某来源中的名次；未出现返回 0。
func (it rrfItem) rankOf(sourceIdx int) int {
	return it.perSource[sourceIdx]
}
