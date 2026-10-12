package retrieve

import (
	"math"
	"testing"
)

// TestFuseRRFSingleSource 单来源退化：RRF 即该来源原序。
func TestFuseRRFSingleSource(t *testing.T) {
	items := fuseRRF([][]string{{"a", "b", "c"}}, 0)
	ids := fuseIDs(items)
	if len(ids) != 3 || ids[0] != "a" || ids[1] != "b" || ids[2] != "c" {
		t.Errorf("单来源应保持原序: %v", ids)
	}
}

// TestFuseRRFMultiSource 双来源融合：两处都出现的条目优先。
func TestFuseRRFMultiSource(t *testing.T) {
	items := fuseRRF([][]string{
		{"a", "b", "c"},
		{"b", "d"},
	}, 0)
	ids := fuseIDs(items)
	// b 在两个来源分别排第 2、第 1：1/61 + 1/60 > a(1/61) > c(1/63)
	if ids[0] != "b" {
		t.Errorf("双来源命中的条目应排最前: %v", ids)
	}
	// 去重：b 只出现一次
	count := 0
	for _, id := range ids {
		if id == "b" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("RRF 应去重: %v", ids)
	}
}

// TestFuseRRFOrdering 排序按融合分降序。
func TestFuseRRFOrdering(t *testing.T) {
	items := fuseRRF([][]string{
		{"x", "y", "z", "w"},
		{"z", "x"},
		{"x"},
	}, 60)
	for i := 1; i < len(items); i++ {
		if items[i].score > items[i-1].score {
			t.Errorf("应降序: [%d]=%v > [%d]=%v", i, items[i].score, i-1, items[i-1].score)
		}
	}
}

// TestFuseRRFScore 分数可验证：rank1 → 1/(k+1)。
func TestFuseRRFScore(t *testing.T) {
	items := fuseRRF([][]string{{"a"}}, 60)
	if math.Abs(items[0].score-1.0/61.0) > 1e-9 {
		t.Errorf("RRF 分不符: %v", items[0].score)
	}
	// 自定义 k
	items2 := fuseRRF([][]string{{"a"}}, 10)
	if math.Abs(items2[0].score-1.0/11.0) > 1e-9 {
		t.Errorf("自定义 k 不符: %v", items2[0].score)
	}
}

// TestFuseRRFEmpty 空来源/空 id 安全。
func TestFuseRRFEmpty(t *testing.T) {
	if got := fuseRRF(nil, 0); len(got) != 0 {
		t.Errorf("空来源应返回空: %v", got)
	}
	if got := fuseRRF([][]string{{}, {"a", ""}}, 0); len(got) != 1 || got[0].id != "a" {
		t.Errorf("空 id 应被忽略: %v", got)
	}
}

// TestRankOf 条目在某来源的名次查询。
func TestRankOf(t *testing.T) {
	items := fuseRRF([][]string{{"a", "b"}, {"b"}}, 60)
	// 找 b
	var b rrfItem
	for _, it := range items {
		if it.id == "b" {
			b = it
		}
	}
	if b.rankOf(0) != 2 || b.rankOf(1) != 1 {
		t.Errorf("rankOf 不符: %d %d", b.rankOf(0), b.rankOf(1))
	}
	if b.rankOf(9) != 0 {
		t.Errorf("未出现的来源应返回 0: %d", b.rankOf(9))
	}
}
