package cache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func vecOf(n int, fill float32) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = fill
	}
	return v
}

// TestSetGetBasic 基本读写。
func TestSetGetBasic(t *testing.T) {
	c := New(Options{TTL: time.Minute, MaxItems: 4})
	want := []float32{0.1, 0.2, 0.3}
	c.Set("bge-m3", 1024, "document", "你好", want)

	got, ok := c.Get("bge-m3", 1024, "document", "你好")
	if !ok {
		t.Fatal("应命中")
	}
	if len(got) != len(want) {
		t.Fatalf("向量长度应为 %d，实际 %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 元素应为 %v，实际 %v", i, want[i], got[i])
		}
	}
}

// TestKeyIncludesAllDimensions 缓存键必须区分所有维度。
//
// 模型、维度、input_type 任一不同都必须是不同的条目 ——
// 混用会导致检索结果彻底失真且无任何报错。
func TestKeyIncludesAllDimensions(t *testing.T) {
	c := New(Options{TTL: time.Minute, MaxItems: 16})
	c.Set("bge-m3", 1024, "document", "文本", vecOf(4, 1))

	miss := []struct {
		name             string
		model, inputType string
		dim              int
	}{
		{"模型不同", "bge-large", "document", 1024},
		{"维度不同", "bge-m3", "document", 768},
		{"input_type 不同", "bge-m3", "query", 1024},
	}
	for _, m := range miss {
		if _, ok := c.Get(m.model, m.dim, m.inputType, "文本"); ok {
			t.Errorf("★ %s 时不应命中", m.name)
		}
	}
	if _, ok := c.Get("bge-m3", 1024, "document", "文本"); !ok {
		t.Error("完全相同的键应命中")
	}
}

// TestGetReturnsCopy 返回值必须是副本。
//
// 调用方通常会把向量塞进 pb 响应或交给下游写库。
// 若返回内部切片，任何原地修改都会静默污染缓存。
func TestGetReturnsCopy(t *testing.T) {
	c := New(Options{TTL: time.Minute, MaxItems: 4})
	c.Set("m", 8, "document", "t", []float32{1, 2, 3, 4})

	got, _ := c.Get("m", 8, "document", "t")
	got[0] = 999
	got = append(got, 555) // 尝试触发append 复用底层数组

	again, _ := c.Get("m", 8, "document", "t")
	if again[0] == 999 {
		t.Error("★ Get 返回的切片被外部修改污染了缓存")
	}
	if len(again) != 4 {
		t.Errorf("★ 缓存中的向量长度被外部 append 改变: %d", len(again))
	}
}

// TestSetCopiesInput 入参也必须是副本。
//
// 调用方复用缓冲区（sync.Pool、预分配切片）是很常见的写法，
// 不拷贝等于把缓存的有效性外包给了调用方。
func TestSetCopiesInput(t *testing.T) {
	c := New(Options{TTL: time.Minute, MaxItems: 4})
	buf := []float32{1, 2, 3, 4}
	c.Set("m", 8, "document", "t", buf)

	buf[0] = 777 // 模拟调用方复用缓冲区
	buf[1] = 888

	got, _ := c.Get("m", 8, "document", "t")
	if got[0] == 777 || got[1] == 888 {
		t.Error("★ 入参切片被后续修改污染了缓存")
	}
	if got[0] != 1 || got[1] != 2 {
		t.Errorf("缓存值应为原始数据，实际 %v", got)
	}
}

// TestNilCacheMethods nil 接收者不应 panic。
//
// embedCache 在配置关闭时就是 nil，
// 上层 Embed 依然会调用 Get/Set/Stats/Clear。
func TestNilCacheMethods(t *testing.T) {
	var c *Cache
	if _, ok := c.Get("m", 8, "q", "t"); ok {
		t.Error("nil 缓存不应命中")
	}
	c.Set("m", 8, "q", "t", []float32{1}) // 不应 panic
	c.Clear()                             // 不应 panic
	if st := c.Stats(); st.Hits != 0 || st.Size != 0 {
		t.Error("nil 缓存统计应为零值")
	}
}

// TestSetIgnoresEmptyVector 空向量不入缓存。
func TestSetIgnoresEmptyVector(t *testing.T) {
	c := New(Options{TTL: time.Minute})
	c.Set("m", 8, "q", "t", nil)
	c.Set("m", 8, "q", "t", []float32{})
	if _, ok := c.Get("m", 8, "q", "t"); ok {
		t.Error("空向量不应入缓存")
	}
	if st := c.Stats(); st.Size != 0 {
		t.Errorf("缓存大小应为 0，实际 %d", st.Size)
	}
}

// TestTTLExpiry TTL 过期后不应命中。
func TestTTLExpiry(t *testing.T) {
	c := New(Options{TTL: 30 * time.Millisecond, MaxItems: 8})
	c.Set("m", 8, "q", "t", []float32{1, 2})

	if _, ok := c.Get("m", 8, "q", "t"); !ok {
		t.Fatal("未过期时应命中")
	}
	time.Sleep(60 * time.Millisecond)

	if _, ok := c.Get("m", 8, "q", "t"); ok {
		t.Error("★ 过期后不应命中")
	}
	// 过期条目应被清理，且计入 miss
	if st := c.Stats(); st.Size != 0 {
		t.Errorf("★ 过期条目应被清理，缓存大小实际 %d", st.Size)
	}
	if st := c.Stats(); st.Misses != 1 {
		t.Errorf("miss 次数应为 1（仅过期那次读），实际 %d", st.Misses)
	}
	if st := c.Stats(); st.Hits != 1 {
		t.Errorf("hit 次数应为 1（过期前那次读），实际 %d", st.Hits)
	}
}

// TestDefaultTTLAndMaxItems 非法的构造参数应回落到默认值。
func TestDefaultTTLAndMaxItems(t *testing.T) {
	c := New(Options{})
	if c.ttl != 24*time.Hour {
		t.Errorf("默认 TTL 应为 24h，实际 %v", c.ttl)
	}
	if c.maxItems != 4096 {
		t.Errorf("默认 MaxItems 应为 4096，实际 %d", c.maxItems)
	}

	c2 := New(Options{TTL: -1, MaxItems: -1})
	if c2.ttl != 24*time.Hour || c2.maxItems != 4096 {
		t.Errorf("负值应回落到默认: ttl=%v max=%d", c2.ttl, c2.maxItems)
	}
}

// TestLRUEviction 超出容量时淘汰最久未使用的。
func TestLRUEviction(t *testing.T) {
	const max = 3
	c := New(Options{TTL: time.Minute, MaxItems: max})

	for i := 0; i < max; i++ {
		c.Set("m", 8, "q", fmt.Sprintf("k%d", i), vecOf(2, float32(i)))
	}
	if st := c.Stats(); st.Size != max {
		t.Fatalf("缓存大小应为 %d，实际 %d", max, st.Size)
	}

	// 触碰 k0 让它变成"最近使用"
	if _, ok := c.Get("m", 8, "q", "k0"); !ok {
		t.Fatal("k0 应命中")
	}

	// 写入第 4 个 →淘汰最久未使用的 k1
	c.Set("m", 8, "q", "k3", vecOf(2, 9))

	if st := c.Stats(); st.Size != max {
		t.Errorf("淘汰后大小应仍为 %d，实际 %d", max, st.Size)
	}
	if _, ok := c.Get("m", 8, "q", "k1"); ok {
		t.Error("★ k1 是最久未使用的，应被淘汰")
	}
	for _, k := range []string{"k0", "k2", "k3"} {
		if _, ok := c.Get("m", 8, "q", k); !ok {
			t.Errorf("%s 不应被淘汰", k)
		}
	}
}

// TestSetOverwrite 重复 Set 同一键应覆盖而非新增。
func TestSetOverwrite(t *testing.T) {
	c := New(Options{TTL: time.Minute, MaxItems: 4})
	c.Set("m", 8, "q", "t", []float32{1, 1})
	c.Set("m", 8, "q", "t", []float32{2, 2, 2})

	if st := c.Stats(); st.Size != 1 {
		t.Errorf("覆盖不应增加条目数，实际 %d", st.Size)
	}
	got, _ := c.Get("m", 8, "q", "t")
	if len(got) != 3 || got[0] != 2 {
		t.Errorf("应被最新值覆盖，实际 %v", got)
	}
}

// TestStats 统计口径。
func TestStats(t *testing.T) {
	c := New(Options{TTL: time.Minute, MaxItems: 8})

	if st := c.Stats(); st.HitRate != 0 {
		t.Errorf("无操作时命中率应为 0，实际 %v", st.HitRate)
	}

	c.Set("m", 8, "q", "t", []float32{1})
	c.Get("m", 8, "q", "t") // hit
	c.Get("m", 8, "q", "x") // miss

	st := c.Stats()
	if st.Hits != 1 || st.Misses != 1 || st.Size != 1 {
		t.Errorf("统计错误: %+v", st)
	}
	if st.HitRate != 0.5 {
		t.Errorf("命中率应为 0.5，实际 %v", st.HitRate)
	}
}

// TestClear 清空缓存并重置统计。
func TestClear(t *testing.T) {
	c := New(Options{TTL: time.Minute, MaxItems: 8})
	c.Set("m", 8, "q", "t", []float32{1})
	c.Get("m", 8, "q", "t") // hit
	c.Get("m", 8, "q", "x") // miss

	c.Clear()

	st := c.Stats()
	if st.Hits != 0 || st.Misses != 0 || st.Size != 0 {
		t.Errorf("Clear 应重置统计: %+v", st)
	}
	if _, ok := c.Get("m", 8, "q", "t"); ok {
		t.Error("Clear 后不应命中")
	}
	// Clear 后仍可正常写入
	c.Set("m", 8, "q", "t2", []float32{2})
	if _, ok := c.Get("m", 8, "q", "t2"); !ok {
		t.Error("Clear 后应可正常写入")
	}
}

// TestConcurrentAccess 并发读写，-race 回归防线。
//
// LRU 的 list.Element 指针在读路径（MoveToFront）与淘汰路径共享，
// 一旦锁范围写错必然触发 race。
func TestConcurrentAccess(t *testing.T) {
	c := New(Options{TTL: 20 * time.Millisecond, MaxItems: 32})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读者
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i := 0; i < 64; i++ {
					c.Get("m", 1024, "document", fmt.Sprintf("k%d", i))
				}
				c.Stats()
			}
		}()
	}

	// 写者
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i := 0; i < 64; i++ {
					c.Set("m", 1024, "document", fmt.Sprintf("k%d", i), vecOf(8, float32(i)))
				}
				if w == 0 {
					c.Clear()
				}
			}
		}(w)
	}

	// TTL 靠时间自然过期，制造过期淘汰与正常淘汰并存的场景
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		c.Set("ttl", 1024, "query", "tick", vecOf(4, 1))
		time.Sleep(2 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	if st := c.Stats(); st.Size > 32 {
		t.Errorf("并发后缓存大小应 ≤ 32，实际 %d", st.Size)
	}
}

// TestConcurrentExternalMutation 外部持有切片时的并发安全。
//
// 每个写者复用自己那一份底层数组（模拟 sync.Pool 式的缓冲区复用），
// 若缓存不拷贝，读写交叉就会读到别人刚写进去的值。
func TestConcurrentExternalMutation(t *testing.T) {
	c := New(Options{TTL: time.Minute, MaxItems: 64})

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			shared := vecOf(16, 0) // 每个写者独立缓冲区
			for i := 0; i < 200; i++ {
				for j := range shared {
					shared[j] = float32(w*10000 + i)
				}
				c.Set("m", 16, "q", fmt.Sprintf("w%d-k%d", w, i), shared)
			}
		}(w)
	}
	wg.Wait()

	// 每个键读回的值必须等于该键对应的写入值
	for w := 0; w < 4; w++ {
		for i := 0; i < 200; i++ {
			got, ok := c.Get("m", 16, "q", fmt.Sprintf("w%d-k%d", w, i))
			if !ok {
				continue // 已被 LRU 淘汰
			}
			want := float32(w*10000 + i)
			if got[0] != want {
				t.Fatalf("键 w%d-k%d 的值被污染: 期望 %v，实际 %v", w, i, want, got[0])
			}
		}
	}
}

// TestItoa 自实现的整数转字符串。
func TestItoa(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"}, {1, "1"}, {9, "9"}, {10, "10"}, {99, "99"},
		{100, "100"}, {1024, "1024"}, {4096, "4096"},
		{-1, "-1"}, {-42, "-42"}, {-1024, "-1024"},
		{2147483647, "2147483647"},
	}
	for _, c := range cases {
		if got := itoa(c.in); got != c.want {
			t.Errorf("itoa(%d) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestKeyFormat 键的拼装格式不应存在歧义。
//
// 模型名与 input_type 来自配置，不是代码常量。
// 用裸分隔符拼接时，"a|b" + "" 与 "a" + "b|" 会拼出同一个键，
// 导致两个不同向量空间的 embedding 被当作同一条 ——
// 静默返回错误向量，没有任何报错，是最难查的一类故障。
func TestKeyFormat(t *testing.T) {
	triples := []struct {
		m, it string
		d     int
	}{
		{"a", "b", 1},
		{"a|b", "", 1}, // 与下一行裸拼接时相同
		{"a", "b|", 1},
		{"ab", "", 1},
		{"a", "", 11},
		{"a", "b|c", 1},
		{"", "", 0},
		{"m", "query", 1024},
	}
	seen := map[string]string{}
	for _, tr := range triples {
		k := key(tr.m, tr.it, "text", tr.d)
		desc := fmt.Sprintf("(%q,%q,%d)", tr.m, tr.it, tr.d)
		if prev, ok := seen[k]; ok {
			t.Errorf("★ 键冲突: %s 与 %s 拼出同一个键 %q", prev, desc, k)
		}
		seen[k] = desc
	}
}
