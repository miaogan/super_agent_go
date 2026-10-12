package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
)

// ---------- 基准：两个引擎必须产出一致的字节 ----------

// testPayload 覆盖容易触发引擎差异的类型组合。
type testPayload struct {
	Name    string            `json:"name"`
	Age     int               `json:"age"`
	Score   float64           `json:"score"`
	Tags    []string          `json:"tags"`
	Meta    map[string]string `json:"meta"`
	HTML    string            `json:"html"`
	Nested  *testNested       `json:"nested"`
	Omit    string            `json:"omit,omitempty"`
	NilMap  map[string]int    `json:"nil_map"`
	NilSl   []int             `json:"nil_slice"`
	Unicode string            `json:"unicode"`
}

type testNested struct {
	A int    `json:"a"`
	B string `json:"b"`
}

func sample() testPayload {
	return testPayload{
		Name:  "aegis",
		Age:   30,
		Score: 99.5,
		// 故意乱序：若某引擎不排序 map key，字节序就会不同
		Meta: map[string]string{"zeta": "1", "alpha": "2", "mu": "3", "beta": "4"},
		Tags: []string{"x", "y", "z"},
		// HTML 敏感字符：EscapeHTML 行为差异的探针
		HTML:    `<script>alert("a&b")</script>`,
		Nested:  &testNested{A: 1, B: "内嵌"},
		Unicode: "中文 \U0001F600 emoji",
	}
}

// TestMarshalBytesIdenticalToStd 是本包最关键的回归锁：
// 本包用 sonic.ConfigStd 而非 ConfigDefault，正是为了保证字节一致。
//
// 一旦有人改成 ConfigDefault（更快但不兼容），这个测试立刻失败。
func TestMarshalBytesIdenticalToStd(t *testing.T) {
	v := sample()
	got, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("stdlib Marshal: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Marshal 字节与 encoding/json 不一致\n got: %s\nwant: %s", got, want)
	}
}

// TestMapKeyOrderStable 验证 map key 排序稳定。
//
// Go 的 map 遍历顺序随机，若引擎不开启 SortMapKeys，
// 同一份数据两次序列化可能得到不同字节 —— 用于签名或缓存键就是灾难。
func TestMapKeyOrderStable(t *testing.T) {
	v := sample()
	first, err := Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := Marshal(v)
		if err != nil {
			t.Fatalf("Marshal #%d: %v", i, err)
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("第 %d 次序列化结果不同，map key 排序不稳定", i)
		}
	}
}

// TestEscapeHTMLAligned 单独锁 HTML 转义这一项。
//
// Sonic 的 ConfigDefault 不转义 < > &，标准库转义。
// 本包声明对齐标准库，因此必须有 \u003c 这类转义序列。
func TestEscapeHTMLAligned(t *testing.T) {
	got, err := Marshal(map[string]string{"h": "<b>&</b>"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(got, []byte(`\u003c`)) {
		t.Errorf("未转义 <，与 encoding/json 行为不一致: %s", got)
	}
}

// TestRoundTrip 覆盖全部公共 API 的往返。
func TestRoundTrip(t *testing.T) {
	orig := sample()
	data, err := Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var back testPayload
	if err := Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(orig, back) {
		t.Errorf("往返后不一致\n got: %+v\nwant: %+v", back, orig)
	}

	// MarshalString / UnmarshalString
	s, err := MarshalString(orig)
	if err != nil {
		t.Fatalf("MarshalString: %v", err)
	}
	if s != string(data) {
		t.Errorf("MarshalString 与 Marshal 不一致\n%s\n%s", s, data)
	}
	var back2 testPayload
	if err := UnmarshalString(s, &back2); err != nil {
		t.Fatalf("UnmarshalString: %v", err)
	}
	if !reflect.DeepEqual(orig, back2) {
		t.Errorf("UnmarshalString 往返不一致")
	}

	// MarshalIndent 应与标准库一致
	ind, err := MarshalIndent(orig, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	wantInd, err := json.MarshalIndent(orig, "", "  ")
	if err != nil {
		t.Fatalf("stdlib MarshalIndent: %v", err)
	}
	if !bytes.Equal(ind, wantInd) {
		t.Errorf("MarshalIndent 与标准库不一致\n got: %s\nwant: %s", ind, wantInd)
	}
}

// TestNilAndEmpty 覆盖 nil 与空集合的编码差异。
//
// Sonic 有个 NoNullSliceOrMap 选项会把 nil slice 编成 [] 而非 null，
// 本包必须保持标准库的 null 语义，否则下游反序列化会拿到非 nil 空切片。
func TestNilAndEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil slice", []int(nil), `null`},
		{"empty slice", []int{}, `[]`},
		{"nil map", map[string]int(nil), `null`},
		{"empty map", map[string]int{}, `{}`},
		{"nil pointer", (*testNested)(nil), `null`},
		{"nil interface", nil, `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Marshal(c.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestUnmarshalSemantics 对齐标准库的反序列化行为。
func TestUnmarshalSemantics(t *testing.T) {
	t.Run("数字默认为 float64", func(t *testing.T) {
		var v any
		if err := UnmarshalString(`{"n":1}`, &v); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		m := v.(map[string]any)
		if _, ok := m["n"].(float64); !ok {
			t.Errorf("interface{} 中的数字应为 float64，实际 %T", m["n"])
		}
	})

	t.Run("大整数解到结构体字段不溢出", func(t *testing.T) {
		// 解到 int64 字段：两个引擎都应精确保留
		var v struct {
			N int64 `json:"n"`
		}
		if err := UnmarshalString(`{"n":9223372036854775807}`, &v); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if v.N != math.MaxInt64 {
			t.Errorf("int64 溢出: got %d", v.N)
		}
	})

	t.Run("大整数解到 interface{} 退化为 float64", func(t *testing.T) {
		// ★ 这不是缺陷而是标准库语义，必须与 sonic 保持一致：
		//   json.Unmarshal 到 any 时，所有数字都是 float64，
		//   int64 上限会变成 9223372036854776000（末位丢失）。
		//
		//   sonic 另有 UseInt64 选项可改变此行为 —— 开启后
		//   同一段 JSON 在两个引擎下解出不同 Go 类型。
		//   本包用 ConfigStd（UseInt64=false）正是为了避开这个差异。
		//
		//   业务含义：若需要精确大整数，必须解到带类型的字段，
		//   不要落到 map[string]any。
		var v any
		if err := UnmarshalString(`{"n":9223372036854775807}`, &v); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		m := v.(map[string]any)
		if _, ok := m["n"].(float64); !ok {
			t.Errorf("interface{} 中的数字应为 float64，实际 %T —— 与标准库不一致", m["n"])
		}
	})

	t.Run("非法 JSON 报错", func(t *testing.T) {
		for _, bad := range []string{`{`, `{"a":}`, `{}extra`, `[1,]`, ``} {
			var v any
			if err := UnmarshalString(bad, &v); err == nil {
				t.Errorf("非法 JSON %q 应报错", bad)
			}
		}
	})

	t.Run("类型不匹配报错", func(t *testing.T) {
		var v testPayload
		if err := UnmarshalString(`{"age":"not-a-number"}`, &v); err == nil {
			t.Error("类型不匹配应报错")
		}
	})

	t.Run("null 解到结构体不报错", func(t *testing.T) {
		// 标准库行为：JSON null 解到 *struct 视为无操作
		var v testPayload
		if err := UnmarshalString(`null`, &v); err != nil {
			t.Errorf("null 应可解: %v", err)
		}
	})
}

// TestValid 与标准库一致
func TestValid(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`{"a":1}`, true},
		{`[1,2,3]`, true},
		{`"str"`, true},
		{`123`, true},
		{`null`, true},
		{`{`, false},
		{``, false},
		{`{}x`, false},
		{`{'a':1}`, false},
	}
	for _, c := range cases {
		if got := Valid([]byte(c.in)); got != c.want {
			t.Errorf("Valid(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// ---------- 流式 API ----------

func TestEncoderDecoder(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Encode(map[string]int{"a": 1}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := enc.Encode(map[string]int{"b": 2}); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// 两个引擎的 Encode 都追加换行，用 Decoder 连续读回
	dec := NewDecoder(bytes.NewReader(buf.Bytes()))
	var m1, m2 map[string]int
	if err := dec.Decode(&m1); err != nil {
		t.Fatalf("Decode 1: %v", err)
	}
	if err := dec.Decode(&m2); err != nil {
		t.Fatalf("Decode 2: %v", err)
	}
	if m1["a"] != 1 || m2["b"] != 2 {
		t.Errorf("流式往返错误: %+v %+v", m1, m2)
	}

	// 第三次应读到 EOF
	var m3 map[string]int
	if err := dec.Decode(&m3); !errors.Is(err, io.EOF) {
		t.Errorf("读完应返回 EOF，实际 %v", err)
	}
}

func TestEncoderSetIndent(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]int{"a": 1}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(buf.String(), "\n") {
		t.Errorf("SetIndent 未生效: %q", buf.String())
	}

	// 与标准库比对
	var stdBuf bytes.Buffer
	stdEnc := json.NewEncoder(&stdBuf)
	stdEnc.SetIndent("", "  ")
	if err := stdEnc.Encode(map[string]int{"a": 1}); err != nil {
		t.Fatalf("stdlib Encode: %v", err)
	}
	if buf.String() != stdBuf.String() {
		t.Errorf("缩进输出与标准库不一致\n got: %q\nwant: %q", buf.String(), stdBuf.String())
	}
}

func TestDecoderOptions(t *testing.T) {
	t.Run("UseNumber", func(t *testing.T) {
		dec := NewDecoder(strings.NewReader(`{"n":1}`))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if _, ok := v.(map[string]any)["n"].(json.Number); !ok {
			t.Errorf("UseNumber 未生效，类型 %T", v.(map[string]any)["n"])
		}
	})

	t.Run("DisallowUnknownFields", func(t *testing.T) {
		dec := NewDecoder(strings.NewReader(`{"unknown":1}`))
		dec.DisallowUnknownFields()
		var v testPayload
		if err := dec.Decode(&v); err == nil {
			t.Error("DisallowUnknownFields 未生效")
		}
	})

	t.Run("Buffered", func(t *testing.T) {
		dec := NewDecoder(strings.NewReader(`{"a":1} {"b":2}`))
		var m map[string]int
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		rest, err := io.ReadAll(dec.Buffered())
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !strings.Contains(string(rest), `"b"`) {
			t.Errorf("Buffered 未返回剩余数据: %q", rest)
		}
	})
}

// ---------- 引擎自检 ----------

// TestEngineInfoConsistent 校验 Info() 的自检信息与实际行为吻合。
//
// 这是「以为用了 sonic 实际没有」这类静默退化的唯一防线。
func TestEngineInfoConsistent(t *testing.T) {
	info := Info()

	if info.Engine != EngineSonic && info.Engine != EngineStdJSON {
		t.Fatalf("未知引擎名 %q", info.Engine)
	}
	if info.Version == "" {
		t.Error("Version 不应为空")
	}
	if info.GOARCH == "" || info.GOOS == "" {
		t.Error("GOARCH/GOOS 不应为空")
	}

	// Native=true 时引擎必须是 sonic；反之若引擎是 sonic 也必须 Native=true
	if info.Native && info.Engine != EngineSonic {
		t.Errorf("Native=true 但引擎是 %s", info.Engine)
	}
	if info.Engine == EngineSonic && !info.Native {
		t.Error("引擎是 sonic 但 Native=false，自相矛盾")
	}

	// 走标准库时应给出回落原因，便于一眼看懂
	if info.Engine == EngineStdJSON && info.FallbackReason == "" {
		t.Error("标准库引擎应说明回落原因")
	}
	if info.Native && info.FallbackReason != "" {
		t.Errorf("原生引擎不应有回落原因，实际 %q", info.FallbackReason)
	}
}

func TestInfoSummaryAndLogFields(t *testing.T) {
	info := Info()

	s := info.Summary()
	for _, want := range []string{"json engine=", "version=", "native=", "goarch="} {
		if !strings.Contains(s, want) {
			t.Errorf("Summary 缺少 %q: %s", want, s)
		}
	}
	if !info.Native && !strings.Contains(s, "reason=") {
		t.Errorf("回落时应带 reason: %s", s)
	}
	// 原因文本含空格时必须加引号，否则日志被切成多列
	if !info.Native && strings.Contains(info.FallbackReason, " ") &&
		!strings.Contains(s, `reason="`) {
		t.Errorf("含空格的回落原因未加引号: %s", s)
	}

	fields := info.LogFields()
	if len(fields)%2 != 0 {
		t.Errorf("LogFields 键值必须成对，实际 %d 项", len(fields))
	}
	keys := map[string]bool{}
	for i := 0; i < len(fields); i += 2 {
		k, ok := fields[i].(string)
		if !ok {
			t.Fatalf("LogFields 奇数位应为 string key，实际 %T", fields[i])
		}
		keys[k] = true
	}
	for _, want := range []string{"json_engine", "json_version", "json_native"} {
		if !keys[want] {
			t.Errorf("LogFields 缺少键 %q", want)
		}
	}
}

func TestNativeSupportedMatchesArch(t *testing.T) {
	// NativeSupported 只应在 amd64/arm64 为真
	got := NativeSupported()
	want := false
	// 借助 runtime 间接验证：不直接 import runtime，用 Info 交叉核对
	if i := Info(); i.GOARCH == "amd64" || i.GOARCH == "arm64" {
		want = true
	}
	_ = want
	// 交叉一致性：若平台非 amd64/arm64 但 NativeSupported 为 true，说明探测逻辑有误
	if i := Info(); i.GOARCH != "amd64" && i.GOARCH != "arm64" && got {
		t.Errorf("非 amd64/arm64 平台不应报告 NativeSupported=true")
	}
	// 若 Info 说原生支持但引擎是标准库，必须是「未加 -tags sonic」所致
	if i := Info(); i.Native && i.Engine != EngineSonic {
		t.Errorf("平台原生支持但未走 sonic: %s", i.Summary())
	}
}

// ---------- 与 sonic.ConfigStd 的显式对照 ----------

// TestEscapeHTMLEscapedForm 确认输出的是标准库那种小写十六进制转义。
//
// \u003c（sonic/std）vs \u003C（大写）都属于合法 JSON，
// 但若两个引擎产出不同大小写，签名/哈希场景仍会不一致。
func TestEscapeHTMLEscapedForm(t *testing.T) {
	got, err := Marshal("<")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want, err := json.Marshal("<")
	if err != nil {
		t.Fatalf("stdlib: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestConcurrentSafety jsonx 被各服务并发调用，必须无竞态。
func TestConcurrentSafety(t *testing.T) {
	const n = 50
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			var err error
			defer func() { done <- err }()
			data, err := Marshal(map[string]int{"i": i})
			if err != nil {
				return
			}
			var back map[string]int
			if err = Unmarshal(data, &back); err != nil {
				return
			}
			if back["i"] != i {
				err = fmt.Errorf("goroutine %d 读到 %d", i, back["i"])
			}
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// TestInfoIdempotent Info() 应可重复调用且结果稳定
func TestInfoIdempotent(t *testing.T) {
	first := Info()
	for i := 0; i < 5; i++ {
		got := Info()
		if got != first {
			t.Fatalf("第 %d 次 Info() 结果不稳定: %s vs %s", i, got.Summary(), first.Summary())
		}
	}
}

// ---------- Benchmark ----------

func BenchmarkMarshalStd(b *testing.B) {
	v := sample()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Marshal(v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshalStd(b *testing.B) {
	data, _ := Marshal(sample())
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var v testPayload
		if err := Unmarshal(data, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshalMap(b *testing.B) {
	v := map[string]any{
		"model": "deepseek-chat", "stream": true, "temperature": 0.7,
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Marshal(v); err != nil {
			b.Fatal(err)
		}
	}
}
