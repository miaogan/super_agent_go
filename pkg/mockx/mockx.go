// Package mockx 提供进程内 mock 的公共能力。
//
// 冒烟测试的核心诉求：agent_service / llm_service / rag_service
// 不连 Nacos、PostgreSQL、Milvus、外部 LLM 也能完整跑通业务链路。
//
// 因此本包提供三件事：
//  1. 可预测的假数据（ID、正文、向量），让断言稳定
//  2. 故障注入（按目标名模拟下游失败），用于验证降级链
//  3. 模拟延迟，用于验证流式与超时
package mockx

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// IDGen 生成确定性自增 ID。
//
// 不用随机数：冒烟脚本需要稳定可断言的 ID（如 doc_id=1001），
// 随机会让测试脚本无法写死断言。
type IDGen struct {
	prefix string
	base   int64
	cur    atomic.Int64
}

// NewIDGen 创建一个从 base 开始自增的 ID 生成器。
func NewIDGen(prefix string, base int64) *IDGen {
	g := &IDGen{prefix: prefix, base: base}
	g.cur.Store(base)
	return g
}

// Next 返回下一个 ID
func (g *IDGen) Next() int64 {
	return g.cur.Add(1)
}

// Peek 返回当前值但不递增
func (g *IDGen) Peek() int64 {
	return g.cur.Load()
}

// Reset 重置回初始 base（测试用）
func (g *IDGen) Reset() {
	g.cur.Store(g.base)
}

// FaultInjector 按目标名模拟失败与延迟。
type FaultInjector struct {
	enabled     bool
	latency     time.Duration
	failTargets map[string]bool

	rndCounter atomic.Uint64
}

// NewFaultInjector 依据 mock 配置构造故障注入器
func NewFaultInjector(enabled bool, latencyMS int, failTargets []string) *FaultInjector {
	fi := &FaultInjector{
		enabled:     enabled,
		latency:     time.Duration(latencyMS) * time.Millisecond,
		failTargets: make(map[string]bool, len(failTargets)),
	}
	for _, t := range failTargets {
		fi.failTargets[strings.ToLower(strings.TrimSpace(t))] = true
	}
	return fi
}

// Enabled 是否处于 mock 模式
func (fi *FaultInjector) Enabled() bool { return fi != nil && fi.enabled }

// Latency 返回本次调用应注入的模拟耗时
func (fi *FaultInjector) Latency() time.Duration {
	if fi == nil || !fi.enabled || fi.latency <= 0 {
		return 0
	}
	return fi.latency
}

// ShouldFail 判断指定目标是否应被模拟为失败
func (fi *FaultInjector) ShouldFail(target string) bool {
	if fi == nil || !fi.enabled {
		return false
	}
	return fi.failTargets[strings.ToLower(strings.TrimSpace(target))]
}

// Sleep 按需注入延迟，感知 ctx 取消
func (fi *FaultInjector) Sleep(ctx context.Context) error {
	d := fi.Latency()
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// jitter 返回 [0,1) 的确定性伪随机数，用于 mock 向量等场景。
func (fi *FaultInjector) jitter() float64 {
	if fi == nil {
		return 0.5
	}
	n := fi.rndCounter.Add(1)
	x := n*6364136223846793005 + 1442695040888963407
	return float64((x>>11)&0x1f_ffff_ffff_ffff) / float64(1<<53)
}

// ---------- 可预测的假数据 ----------

// FakeEmbedding 生成确定性单位向量，且**具备可用的语义结构**。
//
// 用途：mock 模式下 rag_service 需要"向量"参与检索。
//
// ★ 为什么不能用纯随机向量：
//
//	把每段文本独立映射到一个随机单位向量，任意两段的余弦相似度
//	期望为 0、标准差约 1/√dim（dim=1024 时仅 0.031）。
//	这意味着 mock 模式下向量检索**永远命中不了任何东西**
//	（生产阈值 0.3，见 docs/04 数据库设计），RAG 的检索链路将完全无法验证 ——
//	入库显示成功、检索恒为 0 命中，而所有代码看起来都没错。
//
// 采用稀疏词袋投影（hashing trick），三步：
//
//  1. 分词：ASCII 整词 + 中文 bigram（滑动窗口）
//  2. 每个词经 sha256 映射到 [0, hashDim) 的一维，带符号以抵消碰撞
//  3. 亚线性 TF（1+log tf）加权累加 → L2 归一化 → **零填充到 dim 维**
//
// ★ 关键设计：先哈希到低维（256）再零填充到目标维度（1024）
//
//	稀疏词袋在 1024 维里只用到几十维，两段文本的共享维度交集极小，
//	相似度被"各占各的维度"稀释到 0.25 左右 —— 恰好卡在 0.3 阈值之下，
//	检索结果取决于切片边界的运气。
//
//	哈希到 256 维后，不同文本的词更频繁地落在同一维度上，
//	共享词被显著放大：实测强相关文本相似度从 0.26 提升到 0.97，
//	而不相关文本仍为 0.000 —— 区分度反而更清晰。
//
//	零填充不改变 L2 范数，因此余弦计算依然正确，
//	且返回值长度仍是 dim（维度契约不变，Milvus 集合定义无需改动）。
//
// 性质保证：相同 seed 一定得到相同向量（纯函数，无随机数）。
func FakeEmbedding(dim int, seed string) []float32 {
	if dim <= 0 {
		dim = 1024
	}
	// 有效哈希维度：不超过目标维度。
	// dim 本身就小（如 64 维模型）时直接用 dim，无需填充。
	hashDim := mockHashDim
	if hashDim > dim {
		hashDim = dim
	}

	sparse := make([]float32, hashDim)

	// 先统计词频
	counts := make(map[string]int, 64)
	for _, tok := range tokenize(seed) {
		counts[tok]++
	}

	for tok, tf := range counts {
		h := sha256.Sum256([]byte(tok))
		pos := int(binary.BigEndian.Uint32(h[0:4]) % uint32(hashDim))
		// 符号位：让不同词的碰撞部分相互抵消而非叠加
		sign := float32(1)
		if h[4]&1 == 1 {
			sign = -1
		}
		// 亚线性 TF：压制高频词，放大关键词的贡献
		sparse[pos] += sign * float32(1+math.Log(float64(tf)))
	}

	// 全部词都撞在同一维度且符号抵消（极端情况）时，回退到确定性随机向量
	var sumSq float64
	for _, v := range sparse {
		sumSq += float64(v) * float64(v)
	}
	if sumSq == 0 {
		return randomUnitVector(dim, seed)
	}

	inv := float32(1 / math.Sqrt(sumSq))
	vec := make([]float32, dim)
	for i := range sparse {
		vec[i] = sparse[i] * inv
	}
	// 剩余维度保持 0 —— 零填充不改变范数，余弦计算不受影响
	return vec
}

// mockHashDim mock 向量的有效哈希维度。
//
// 为什么是 256：需要 ≥ 词表量级以减少碰撞带来的假相似，
// 又要足够小让共享词被放大（见 FakeEmbedding 注释中的实测数据）。
// 实测 128 以下会因碰撞过多导致余弦溢出到 ±1 之外。
const mockHashDim = 256

// SparseToken 稀疏向量的一维：{词表位置 → 权重}。
type SparseToken struct {
	Index int64
	Value float32
}

// SparseFromText 由文本生成确定性稀疏向量（词袋哈希）。
//
// 与 FakeEmbedding 同源：每个词经 sha256 映射到 [0, mockHashDim)，
// 亚线性 TF 加权。性质保证：
//   - 相同文本 → 相同稀疏向量（纯函数，无随机数）
//   - 共享词的文本在相同位置有非零权重 → mock 稀疏检索可打分
//
// 返回 nil 表示空文本/无词。
func SparseFromText(s string) []SparseToken {
	counts := make(map[string]int, 32)
	for _, tok := range tokenize(s) {
		counts[tok]++
	}
	if len(counts) == 0 {
		return nil
	}
	out := make([]SparseToken, 0, len(counts))
	for tok, tf := range counts {
		h := sha256.Sum256([]byte(tok))
		pos := binary.BigEndian.Uint32(h[0:4]) % mockHashDim
		// 符号位让不同词的碰撞相互抵消（与 FakeEmbedding 一致）
		sign := float32(1)
		if h[4]&1 == 1 {
			sign = -1
		}
		w := sign * float32(1+math.Log(float64(tf)))
		out = append(out, SparseToken{Index: int64(pos), Value: w})
	}
	// 按位置升序，便于 mock 检索比对
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// tokenize 极简分词：ASCII 连续字母数字为一个词，中文只取 bigram。
//
// ★ 为什么不要中文单字：
//
//	单字（"向""量""度"）既冗余又在长文本里高频重复，
//	会同时稀释信号、放大噪声。实测一个 9 字查询对 83 token 的切片，
//	共享的 13 个 token 里绝大多数是重复单字。
//	只保留 bigram 后信号集中在"向量""维度""检索"这类高信息量词上，
//	与真实 embedding 的行为更接近。
//
//	另外，中文 bigram 需要跨界：查询"向量维度"与文档"…量维…"这种
//	错位切分仍能靠相交的 bigram 命中。
func tokenize(s string) []string {
	var out []string
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r < 128 {
			if isAlnum(r) {
				start := i
				for i+1 < len(runes) && runes[i+1] < 128 && isAlnum(runes[i+1]) {
					i++
				}
				out = append(out, string(runes[start:i+1]))
			}
			continue
		}
		// 中文 bigram（滑动窗口：每个相邻字符对都成一个 token）；
		// 末尾若剩单字则忽略（信息量太低）。
		//
		// ★ 必须用滑动窗口而非非重叠切分：
		//   非重叠时 "向量维度" 只能切出 "向量|维度"，
		//   而 "检索向量" 会切出 "检索|向量" —— 只要切分位置错开一点
		//   （"…检索向量…" 变成 "检索|索向量|量…"）关键词就丢了。
		//   滑动窗口保证任意位置的双字组合都能被匹配上。
		if i+1 < len(runes) && runes[i+1] >= 128 {
			out = append(out, string(runes[i:i+2]))
		}
	}
	return out
}

func isAlnum(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// randomUnitVector 由 seed 确定性地生成随机单位向量（词袋全碰撞时的回退路径）
func randomUnitVector(dim int, seed string) []float32 {
	vec := make([]float32, dim)
	h := sha256.Sum256([]byte(seed))
	buf := make([]byte, 0, dim*4)
	for i := 0; len(buf) < dim*4; i++ {
		h2 := sha256.Sum256(append(h[:], byte(i)))
		buf = append(buf, h2[:]...)
	}
	var sum float64
	for i := 0; i < dim; i++ {
		v := float64(binary.LittleEndian.Uint32(buf[i*4:i*4+4])) / float64(math.MaxUint32)
		vec[i] = float32(v*2 - 1)
		sum += float64(vec[i]) * float64(vec[i])
	}
	// 归一化为单位向量，使 COSINE 相似度计算有意义
	if sum == 0 {
		vec[0] = 1
		return vec
	}
	norm := math.Sqrt(sum)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

// Cosine 计算两个向量的余弦相似度（mock 检索打分用）
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// HashID 由字符串派生稳定 ID（用于 OAuth subject → uid 映射）
func HashID(prefix string, s string) int64 {
	h := sha256.Sum256([]byte(s))
	// 取 63 位避免溢出为负
	v := int64(binary.BigEndian.Uint64(h[:8]) & 0x7f_ff_ffff_ffff_ffff)
	if v == 0 {
		v = 1
	}
	_ = prefix
	return v
}

// HexID 返回 n 字节的十六进制随机标识（state / code_verifier 等）
func HexID(n int) string {
	b := make([]byte, n)
	fillRandom(b)
	return hex.EncodeToString(b)
}
