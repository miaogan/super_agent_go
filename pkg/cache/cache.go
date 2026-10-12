// Package cache 实现 Embedding 结果缓存。
//
// 为什么需要：向量化是 RAG 中最贵的一环。
// 同一段文档在多次入库/重试中会被反复向量化，缓存能显著降低成本。
//
// 选型说明：
//
//	当前为进程内 LRU —— 零依赖、单实例有效。
//	多实例部署时命中率会下降，需要换成 Redis（接口已按此设计）。
//	失效策略用 TTL，不做主动失效：Embedding 结果是纯函数，
//	只要模型名与维度不变，缓存内容永远正确。
package cache

import (
	"container/list"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stats 缓存统计
type Stats struct {
	Hits    int64
	Misses  int64
	Size    int
	HitRate float64
}

// Cache LRU + TTL 缓存
type Cache struct {
	mu       sync.Mutex
	ttl      time.Duration
	maxItems int

	ll    *list.List               // front = 最近使用
	items map[string]*list.Element // key → element

	hits   int64
	misses int64
}

// entry 缓存条目
type entry struct {
	key       string
	vec       []float32
	expiresAt time.Time
}

// Options 构造参数
type Options struct {
	TTL      time.Duration
	MaxItems int
}

// New 构造缓存。MaxItems <= 0 时默认 4096。
func New(opt Options) *Cache {
	if opt.TTL <= 0 {
		opt.TTL = 24 * time.Hour
	}
	if opt.MaxItems <= 0 {
		opt.MaxItems = 4096
	}
	return &Cache{
		ttl:      opt.TTL,
		maxItems: opt.MaxItems,
		ll:       list.New(),
		items:    make(map[string]*list.Element, opt.MaxItems),
	}
}

// key 生成缓存键。
//
// 必须包含模型名：不同模型的向量空间完全不兼容，
// 混用会导致检索结果彻底失真且难以排查。
//
// ★ 各段用长度前缀而非裸分隔符拼接。
//
//	模型名/ input_type 来自配置，不是代码里的常量，
//	一旦某个模型名里含有分隔符（如 "a|b"），
//	裸拼接就会与 ("a", "b") 产生同一个键 —— 两个不同向量空间的
//	embedding 被判为同一条，静默返回错向量且不报错。
//	长度前缀让编码天然无歧义，代价只是几个字节。
func key(model, inputType, text string, dim int) string {
	var b strings.Builder
	b.Grow(len(model) + len(inputType) + len(text) + 24)
	writePart(&b, model)
	writePart(&b, inputType)
	b.WriteString(strconv.Itoa(dim))
	b.WriteByte(0)
	writePart(&b, text)
	return b.String()
}

func writePart(b *strings.Builder, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
	b.WriteByte(0)
}

// Get 读取缓存。
//
// ★ 返回的是副本，不是内部切片。
//
//	caller 通常会把向量塞进 pb 响应、或交给下游做归一化/写库。
//	若直接返回内部切片，外部的任何原地修改都会静默污染缓存 ——
//	之后每次命中都返回被改坏的向量，且不报错、极难排查。
//	4KB 的拷贝相对于一次 Embedding 网络调用可忽略，不值得为此冒险。
func (c *Cache) Get(model string, dim int, inputType, text string) ([]float32, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	k := key(model, inputType, text, dim)
	el, ok := c.items[k]
	if !ok {
		c.misses++
		return nil, false
	}
	e := el.Value.(*entry)
	if time.Now().After(e.expiresAt) {
		c.removeElement(el)
		c.misses++
		return nil, false
	}
	c.ll.MoveToFront(el)
	c.hits++
	return cloneVec(e.vec), true
}

// Set 写入缓存。
//
// ★ 同样存副本：调用方复用缓冲区（sync.Pool、预分配切片）
//
//	是很常见的写法，不拷贝等于把缓存的有效性外包给了调用方。
func (c *Cache) Set(model string, dim int, inputType, text string, vec []float32) {
	if c == nil || len(vec) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	k := key(model, inputType, text, dim)
	if el, ok := c.items[k]; ok {
		e := el.Value.(*entry)
		e.vec = cloneVec(vec)
		e.expiresAt = time.Now().Add(c.ttl)
		c.ll.MoveToFront(el)
		return
	}

	el := c.ll.PushFront(&entry{
		key:       k,
		vec:       cloneVec(vec),
		expiresAt: time.Now().Add(c.ttl),
	})
	c.items[k] = el

	// 淘汰最久未使用的
	for c.ll.Len() > c.maxItems {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.removeElement(back)
	}
}

func cloneVec(v []float32) []float32 {
	out := make([]float32, len(v))
	copy(out, v)
	return out
}

// Stats 返回统计信息
func (c *Cache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	total := c.hits + c.misses
	rate := 0.0
	if total > 0 {
		rate = float64(c.hits) / float64(total)
	}
	return Stats{
		Hits:    c.hits,
		Misses:  c.misses,
		Size:    c.ll.Len(),
		HitRate: rate,
	}
}

// Clear 清空缓存（配置热更新时调用）
func (c *Cache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.items = make(map[string]*list.Element, c.maxItems)
	c.hits, c.misses = 0, 0
}

func (c *Cache) removeElement(el *list.Element) {
	c.ll.Remove(el)
	delete(c.items, el.Value.(*entry).key)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
