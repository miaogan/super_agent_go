package pagination

import (
	"testing"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
)

// TestNormalizeDefaults 缺省与非法值应回落到默认。
func TestNormalizeDefaults(t *testing.T) {
	cases := []struct {
		name     string
		req      *commonv1.PageReq
		wantPage int
		wantSize int
	}{
		{"nil 请求", nil, 1, 20},
		{"零值", &commonv1.PageReq{}, 1, 20},
		{"负数", &commonv1.PageReq{Page: -5, PageSize: -10}, 1, 20},
		{"正常值", &commonv1.PageReq{Page: 3, PageSize: 50}, 3, 50},
		{"超大页", &commonv1.PageReq{Page: 1, PageSize: 100000}, 1, 200},
		{"边界 200", &commonv1.PageReq{Page: 1, PageSize: 200}, 1, 200},
		{"边界 201", &commonv1.PageReq{Page: 1, PageSize: 201}, 1, 200},
	}
	for _, c := range cases {
		page, size := Normalize(c.req)
		if page != c.wantPage || size != c.wantSize {
			t.Errorf("%s: Normalize = (%d, %d)，期望 (%d, %d)",
				c.name, page, size, c.wantPage, c.wantSize)
		}
	}
}

// TestNormalizeCapsPageSize ★ 防拖垮数据库。
//
// PageSize 由客户端完全控制，不封顶时一次 page_size=1000000
// 就能让 PG 拉取整表，内存与响应体同时爆炸。
func TestNormalizeCapsPageSize(t *testing.T) {
	for _, huge := range []int32{201, 1000, 100000, 2147483647} {
		_, size := Normalize(&commonv1.PageReq{Page: 1, PageSize: huge})
		if size != maxPageSize {
			t.Errorf("pageSize=%d 应被截断到 %d，实际 %d", huge, maxPageSize, size)
		}
	}
}

// TestOffset OFFSET 计算。
func TestOffset(t *testing.T) {
	cases := []struct {
		req  *commonv1.PageReq
		want int
	}{
		{nil, 0},
		{&commonv1.PageReq{}, 0},
		{&commonv1.PageReq{Page: 1, PageSize: 20}, 0},
		{&commonv1.PageReq{Page: 2, PageSize: 20}, 20},
		{&commonv1.PageReq{Page: 5, PageSize: 10}, 40},
		{&commonv1.PageReq{Page: 3}, 40}, // size 用默认 20
	}
	for _, c := range cases {
		if got := Offset(c.req); got != c.want {
			t.Errorf("Offset(%v) = %d，期望 %d", c.req, got, c.want)
		}
	}
}

// TestMeta 分页元信息。
func TestMeta(t *testing.T) {
	cases := []struct {
		name    string
		req     *commonv1.PageReq
		total   int
		wantHas bool
		wantTot int32
	}{
		{"首页未完", &commonv1.PageReq{Page: 1, PageSize: 10}, 25, true, 25},
		{"正好最后一页", &commonv1.PageReq{Page: 3, PageSize: 10}, 30, false, 30},
		{"最后一页部分", &commonv1.PageReq{Page: 3, PageSize: 10}, 25, false, 25},
		{"空结果", &commonv1.PageReq{Page: 1, PageSize: 10}, 0, false, 0},
		{"页码超出", &commonv1.PageReq{Page: 99, PageSize: 10}, 25, false, 25},
	}
	for _, c := range cases {
		m := Meta(c.req, c.total)
		if m == nil {
			t.Fatalf("%s: Meta 返回 nil", c.name)
		}
		if m.HasMore != c.wantHas {
			t.Errorf("%s: HasMore = %v，期望 %v（total=%d page=%d size=%d）",
				c.name, m.HasMore, c.wantHas, c.total, m.Page, m.PageSize)
		}
		if m.Total != c.wantTot {
			t.Errorf("%s: Total = %d，期望 %d", c.name, m.Total, c.wantTot)
		}
	}
}

// TestMetaHasMoreSemantics HasMore 的边界语义。
//
// page*size == total 时应恰好为 false —— 多给一次空页
// 会让前端多发一次必然无结果的请求。
func TestMetaHasMoreSemantics(t *testing.T) {
	if m := Meta(&commonv1.PageReq{Page: 2, PageSize: 10}, 20); m.HasMore {
		t.Error("正好整除时 HasMore 应为 false")
	}
	if m := Meta(&commonv1.PageReq{Page: 2, PageSize: 10}, 21); !m.HasMore {
		t.Error("还有余量时 HasMore 应为 true")
	}
}

// TestSliceBasic 内存切片分页。
func TestSliceBasic(t *testing.T) {
	items := make([]int, 25)
	for i := range items {
		items[i] = i
	}

	page, total := Slice(items, &commonv1.PageReq{Page: 1, PageSize: 10})
	if total != 25 {
		t.Errorf("total 应为 25，实际 %d", total)
	}
	if len(page) != 10 || page[0] != 0 {
		t.Errorf("首页应返回 [0..9]，实际 %v", page)
	}

	page, _ = Slice(items, &commonv1.PageReq{Page: 3, PageSize: 10})
	if len(page) != 5 {
		t.Errorf("末页应返回 5 条，实际 %d", len(page))
	}
	if page[0] != 20 || page[4] != 24 {
		t.Errorf("末页内容错误: %v", page)
	}
}

// TestSliceOutOfRange 页码超出范围。
func TestSliceOutOfRange(t *testing.T) {
	items := []int{1, 2, 3}
	page, total := Slice(items, &commonv1.PageReq{Page: 99, PageSize: 10})
	if total != 3 {
		t.Errorf("total 应仍为真实总数 3，实际 %d", total)
	}
	if len(page) != 0 {
		t.Errorf("越界页应返回空切片，实际 %v", page)
	}
	if page == nil {
		t.Error("越界时应返回非 nil 空切片，便于调用方安全遍历")
	}
}

// TestSliceEmpty 空数据源。
func TestSliceEmpty(t *testing.T) {
	page, total := Slice([]int{}, &commonv1.PageReq{Page: 1, PageSize: 10})
	if total != 0 || len(page) != 0 {
		t.Errorf("空数据源应返回 (空, 0)，实际 (%v, %d)", page, total)
	}
	page, total = Slice[int](nil, nil)
	if total != 0 || len(page) != 0 {
		t.Errorf("nil 输入应返回 (空, 0)，实际 (%v, %d)", page, total)
	}
}

// TestSliceNilReq nil 请求走默认值。
func TestSliceNilReq(t *testing.T) {
	items := make([]int, 30)
	page, total := Slice(items, nil)
	if total != 30 {
		t.Errorf("total 应为 30，实际 %d", total)
	}
	if len(page) != 20 {
		t.Errorf("默认页大小应为 20，实际 %d", len(page))
	}
}

// TestSliceStruct 泛型对结构体同样生效。
func TestSliceStruct(t *testing.T) {
	type user struct {
		ID   int64
		Name string
	}
	users := make([]user, 12)
	for i := range users {
		users[i] = user{ID: int64(i), Name: "u"}
	}
	page, total := Slice(users, &commonv1.PageReq{Page: 2, PageSize: 5})
	if total != 12 {
		t.Errorf("total 应为 12，实际 %d", total)
	}
	if len(page) != 5 || page[0].ID != 5 {
		t.Errorf("第二页应为 ID 5..9，实际 %+v", page)
	}
}

// TestParseInt 字符串解析。
func TestParseInt(t *testing.T) {
	cases := []struct {
		in   string
		def  int
		want int
	}{
		{"", 7, 7},
		{"42", 7, 42},
		{"0", 7, 0},
		{"-1", 7, -1},
		{"abc", 7, 7},
		{"1.5", 7, 7},
		{"  8", 7, 7},                     // 有空格：Atoi 拒绝
		{"1e3", 7, 7},                     // 科学计数法不接受
		{"99999999999999999999999", 7, 7}, // 溢出
	}
	for _, c := range cases {
		if got := ParseInt(c.in, c.def); got != c.want {
			t.Errorf("ParseInt(%q, %d) = %d，期望 %d", c.in, c.def, got, c.want)
		}
	}
}

// TestSliceNormalizeConsistency Slice 与 Normalize 必须用同一套语义。
//
// mock 仓储（Slice）与生产 PG 仓储（Offset+Limit）共用 Normalize，
// 若这里出现分歧，同一个请求在mock 下能翻页、在生产下就会错。
func TestSliceNormalizeConsistency(t *testing.T) {
	// 数据量足够大，确保第 3 页确实有数据
	items := make([]int, 1000)
	req := &commonv1.PageReq{Page: 3, PageSize: 100000}

	_, size := Normalize(req)
	page, total := Slice(items, req)

	wantLen := size
	if len(page) != wantLen {
		t.Errorf("Slice 长度 %d 与 Normalize 的页大小 %d 不一致", len(page), wantLen)
	}
	if total != len(items) {
		t.Errorf("total %d 应为完整数据量 %d（分页不影响总数）", total, len(items))
	}
	if got := Offset(req); got != (3-1)*size {
		t.Errorf("Offset %d 与 (page-1)*size %d 不一致", got, (3-1)*size)
	}
}
