package milvus3

import (
	"errors"
	"strings"
	"testing"

	"github.com/aegis/pkg/vectordb"
)

// ---------- buildFilter ----------
//
// 这一层是纯字符串拼装，最容易出错也最难靠集成测试发现：
// 表达式写错时 Milvus 不报「表达式非法」，而是返回空结果 ——
// 表现为「搜不到任何东西」，极难定位。所以必须单测锁死。

func TestBuildFilterEmptyWhenNoCondition(t *testing.T) {
	// 无任何过滤条件时必须返回空串：
	// 返回 "true" 或 "1 == 1" 之类的非空表达式虽然也能跑，
	// 但会让 Milvus 走过滤分支、放弃某些优化路径。
	if got := buildFilter(&vectordb.SearchRequest{}); got != "" {
		t.Fatalf("无条件时应返回空串，实际 %q", got)
	}
}

func TestBuildFilterKBIDs(t *testing.T) {
	got := buildFilter(&vectordb.SearchRequest{KBIDs: []int64{7, 9}})
	want := "(kb_id == 7 or kb_id == 9)"
	if got != want {
		t.Fatalf("kb过滤表达式 =\n%q\n期望\n%q", got, want)
	}
}

func TestBuildFilterSingleKBID(t *testing.T) {
	got := buildFilter(&vectordb.SearchRequest{KBIDs: []int64{7}})
	if got != "(kb_id == 7)" {
		t.Fatalf("单个 kb 应生成单个子句，实际 %q", got)
	}
}

// ★ 这条是最容易被写错的：KB 过滤是「或」，
// 若误写成「且」，多知识库检索会一个都匹配不上。
func TestBuildFilterMultiKBIsOrNotAnd(t *testing.T) {
	got := buildFilter(&vectordb.SearchRequest{KBIDs: []int64{1, 2}})
	if !strings.Contains(got, " or ") {
		t.Fatalf("多知识库必须是「或」关系，实际 %q", got)
	}
	if strings.Contains(got, "kb_id == 1) and") {
		t.Fatalf("多知识库不应出现「且」，实际 %q", got)
	}
}

func TestBuildFilterExcludeDocIDsIsAnd(t *testing.T) {
	got := buildFilter(&vectordb.SearchRequest{ExcludeDocIDs: []int64{3, 4}})
	// 排除是「且」：doc_id != 3 且 != 4 才是「都排除」
	want := "(doc_id != 3 and doc_id != 4)"
	if got != want {
		t.Fatalf("排除表达式 =\n%q\n期望\n%q", got, want)
	}
}

// ★ Tenant 必须兼容空值。
//
// 背景：ingest 流水线目前不写 Tenant，存量数据该字段是空串。
// 若按严格等值过滤，检索会全部落空且不报错——
// 这是本适配层最容易埋下的「静默失效」陷阱。
func TestBuildFilterTenantAllowsEmptyLegacyValue(t *testing.T) {
	got := buildFilter(&vectordb.SearchRequest{Tenant: "acme"})
	want := `(tenant == "" or tenant == "acme")`
	if got != want {
		t.Fatalf("tenant 过滤 =\n%q\n期望\n%q", got, want)
	}
	if !strings.Contains(got, `tenant == ""`) {
		t.Fatalf("必须兼容 tenant 为空的历史数据，实际 %q", got)
	}
}

func TestBuildFilterNoTenantWhenEmpty(t *testing.T) {
	// 空 tenant 不该产生任何 tenant 条件：
	// 空串意味着「不过滤」，而不是「只匹配空值」。
	got := buildFilter(&vectordb.SearchRequest{})
	if strings.Contains(got, "tenant") {
		t.Fatalf("空 tenant 不应生成条件，实际 %q", got)
	}
}

func TestBuildFilterCombinesWithAnd(t *testing.T) {
	got := buildFilter(&vectordb.SearchRequest{
		KBIDs:         []int64{1},
		Tenant:        "t1",
		ExcludeDocIDs: []int64{5},
	})
	// 三段条件之间必须是「且」
	if strings.Count(got, " and ") != 2 {
		t.Fatalf("三个条件应用两个「且」连接，实际 %q", got)
	}
}

// ---------- quoteStr ----------

// ★ 注入防护：chunk_id 若含引号，未转义会让整个表达式变形。
func TestQuoteStrEscapesQuotes(t *testing.T) {
	got := quoteStr(`a"b`)
	// 首尾各一个定界引号，内部引号被转义成\"
	if strings.Count(got, `"`) != 3 {
		t.Fatalf("引号计数应为 3（首尾 + 被转义的原文引号），实际 %d: %q",
			strings.Count(got, `"`), got)
	}
	if !strings.Contains(got, `\"`) {
		t.Fatalf("内部引号应被转义，实际 %q", got)
	}
	if strings.HasPrefix(got, `""`) {
		t.Fatalf("不应产生空引号对，实际 %q", got)
	}
}

func TestQuoteStrEscapesBackslash(t *testing.T) {
	got := quoteStr(`a\b`)
	if !strings.Contains(got, `\\`) {
		t.Fatalf("反斜杠应被转义，实际 %q", got)
	}
}

func TestQuoteStrPlain(t *testing.T) {
	if got := quoteStr("chunk-1"); got != `"chunk-1"` {
		t.Fatalf("普通字符串应只加引号，实际 %q", got)
	}
}

// ---------- classifyErr ----------

// ★ 分类错误决定降级行为，判错等于把故障当正常或把正常当故障：
//   - 误判为 ErrUnavailable → 真故障被静默降级，故障被掩盖
//   - 误判为其他 → 降级链不触发，用户直接看到 5xx

func TestClassifyErrNil(t *testing.T) {
	if err := classifyErr(nil); err != nil {
		t.Fatalf("nil 应返回 nil，实际 %v", err)
	}
}

func TestClassifyErrUnavailable(t *testing.T) {
	cases := []string{
		"rpc error: code = Unavailable desc = connection refused",
		"context deadline exceeded",
		"transport is closing",
		"connection reset by peer",
	}
	for _, msg := range cases {
		err := classifyErr(errors.New(msg))
		if !errors.Is(err, vectordb.ErrUnavailable) {
			t.Fatalf("%q 应归为 ErrUnavailable，实际 %v", msg, err)
		}
	}
}

func TestClassifyErrDimMismatch(t *testing.T) {
	err := classifyErr(errors.New("vector dimension mismatch"))
	if !errors.Is(err, vectordb.ErrDimMismatch) {
		t.Fatalf("维度错误应归为 ErrDimMismatch，实际 %v", err)
	}
}

// 一个普通错误必须落到 ErrSearchFailed，
// 不能误判成 ErrUnavailable —— 否则任何小问题都会触发降级。
func TestClassifyErrGenericIsSearchFailed(t *testing.T) {
	err := classifyErr(errors.New("something odd happened"))
	if !errors.Is(err, vectordb.ErrSearchFailed) {
		t.Fatalf("普通错误应归为 ErrSearchFailed，实际 %v", err)
	}
	if errors.Is(err, vectordb.ErrUnavailable) {
		t.Fatal("普通错误不应被归为 ErrUnavailable（否则会误触发降级）")
	}
}

// ---------- metricByName ----------

func TestMetricByName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"COSINE", "COSINE"},
		{"cosine", "COSINE"},   // 小写
		{" COSINE ", "COSINE"}, // 带空格
		{"L2", "L2"},
		{"IP", "IP"},
		{"", "COSINE"},        // 空值兜底
		{"unknown", "COSINE"}, // 未知兜底
	}
	for _, c := range cases {
		got := string(metricByName(c.in))
		if got != c.want {
			t.Fatalf("metricByName(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// ---------- 常量一致性 ----------

// content 上限写进 schema 就固化了。
// 若常量与实际写入检查不一致，就会出现「以为没超、实际被 Milvus 拒」
// 或「以为会拒、实际被静默截断」两种难查的问题。
func TestContentMaxLenMatchesWriteCheck(t *testing.T) {
	if contentFieldMaxLen != 65535 {
		t.Fatalf("content max_length = %d，期望 65535（Milvus VarChar 单值上限）",
			contentFieldMaxLen)
	}
}

func TestFieldNames(t *testing.T) {
	// 字段名是写进存量数据的契约，改名等于数据全部失联。
	// 一旦有人改动，这里会拦住。
	want := map[string]string{
		fieldChunkID:    "chunk_id",
		fieldVector:     "vector",
		fieldKBID:       "kb_id",
		fieldDocID:      "doc_id",
		fieldSeq:        "seq",
		fieldContent:    "content",
		fieldPageNo:     "page_no",
		fieldTokenCount: "token_count",
		fieldTenant:     "tenant",
	}
	got := map[string]string{
		"chunk_id": fieldChunkID, "vector": fieldVector, "kb_id": fieldKBID,
		"doc_id": fieldDocID, "seq": fieldSeq, "content": fieldContent,
		"page_no": fieldPageNo, "token_count": fieldTokenCount, "tenant": fieldTenant,
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("字段 %s = %q，期望 %q（字段名变更会导致存量数据不可读）", k, got[k], v)
		}
	}
}
