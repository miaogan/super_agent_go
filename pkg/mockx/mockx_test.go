package mockx

import "testing"

// TestFakeEmbeddingSemantic是 mock 检索链路的回归防线。
//
// 背景：曾把 FakeEmbedding 实现为"文本 → 纯随机单位向量"，
// 结果 1024 维随机向量的余弦期望 0、标准差仅 0.031，
// 永远达不到 0.3 的检索阈值 —— mock 模式下 RAG 检索恒为 0 命中，
// 而入库、编译、所有代码看上去都正常。这类 bug 只能靠测试拦住。
//
// ★ 关于断言阈值：
//
//	稀疏词袋的相似度有数学上限 ≈ 共享 token 数 / √(len_q × len_d)。
//	典型场景（8 token 查询 vs 42 token 切片，共享 5 个）上限约 0.24 ——
//	再怎么调参也突破不了。因此这里断言的是"有意义的区分度"，
//	而非生产级绝对分数；生产阈值由 RAG 标定后写入
//	knowledge_bases.score_threshold（默认 0.3，见 docs/04），
//	mock 模式另用 conf.RAG.MockScoreThreshold（0.1）。
func TestFakeEmbeddingSemantic(t *testing.T) {
	const dim = 1024
	const mockThreshold = 0.1

	query := "Milvus 的向量维度是多少"
	qv := FakeEmbedding(dim, query)
	if len(qv) != dim {
		t.Fatalf("维度错误：期望 %d，实际 %d", dim, len(qv))
	}

	strong := Cosine(qv, FakeEmbedding(dim,
		"知识库的向量检索使用 Milvus 3.0.2，向量检索是 RAG 的核心环节"))
	// 真实切片长度（约 42 token），曾经因词袋稀释只有 0.11
	longDoc := Cosine(qv, FakeEmbedding(dim,
		"Embedding 模型使用 bge-m3，向量维度为 1024，度量方式为 COSINE。"+
			"检索支持三级降级：Milvus 向量检索、PostgreSQL 关键词检索、纯 LLM 对话。"))
	// 共享"平台"但无关键词重叠
	partial := Cosine(qv, FakeEmbedding(dim, "平台部署在 K8s，网关负责路由到各微服务"))
	weak := Cosine(qv, FakeEmbedding(dim, "今天天气不错，适合去公园散步喝茶"))

	t.Logf("强相关=%.4f 长切片=%.4f 弱相关=%.4f 无关=%.4f",
		strong, longDoc, partial, weak)

	// 断言 1：强相关必须超过 mock 检索阈值，否则冒烟无法命中
	if strong <= mockThreshold {
		t.Errorf("强相关相似度 %.4f 未超过 mock 阈值 %.2f，检索将无法命中", strong, mockThreshold)
	}
	// 断言 2：真实长度的切片同样要可用
	if longDoc <= mockThreshold {
		t.Errorf("长切片相似度 %.4f 未超过 mock 阈值 %.2f，真实切片将检索不到", longDoc, mockThreshold)
	}
	// 断言 3：无关文本必须为低相似度，否则满屏误召回
	if weak > mockThreshold {
		t.Errorf("无关文本相似度 %.4f 超过阈值 %.2f，会造成误召回", weak, mockThreshold)
	}
	// 断言 4：必须有区分度 —— 强相关显著高于无关
	if strong <= weak*3 {
		t.Errorf("区分度不足：强相关 %.4f，无关 %.4f", strong, weak)
	}
	// 断言 5：弱相关不应高于强相关
	if partial > strong {
		t.Errorf("弱相关(%.4f)不应高于强相关(%.4f)", partial, strong)
	}
}

// TestFakeEmbeddingDeterministic 保证它是纯函数：
// Embedding 缓存与文档去重都依赖这一点。
func TestFakeEmbeddingDeterministic(t *testing.T) {
	text := "重复入库的文档必须得到相同向量，否则缓存与去重都会失效"
	a := FakeEmbedding(1024, text)
	b := FakeEmbedding(1024, text)
	if cos := Cosine(a, b); cos < 0.9999 {
		t.Errorf("同文本两次向量化不一致，cos=%.6f", cos)
	}
}

// TestFakeEmbeddingUnitNorm 确认已归一化，
// 否则 Cosine 的分母补偿会掩盖维度错误。
func TestFakeEmbeddingUnitNorm(t *testing.T) {
	for _, text := range []string{"", "短", "较长的一段中文文本内容用于测试归一化"} {
		vec := FakeEmbedding(1024, text)
		var sum float64
		for _, v := range vec {
			sum += float64(v) * float64(v)
		}
		if sum < 0.99 || sum > 1.01 {
			t.Errorf("text=%q 向量未归一化，平方和=%.4f", text, sum)
		}
	}
}

// TestFakeEmbeddingDimContract 确认不同维度下都能工作，
// 且返回值长度始终等于请求维度（Milvus 集合定义依赖该契约）。
func TestFakeEmbeddingDimContract(t *testing.T) {
	for _, dim := range []int{64, 256, 768, 1024, 1536} {
		vec := FakeEmbedding(dim, "维度契约测试")
		if len(vec) != dim {
			t.Errorf("dim=%d 返回长度=%d", dim, len(vec))
		}
	}
	// 非法维度应回退到 1024，而不是 panic
	if got := len(FakeEmbedding(0, "x")); got != 1024 {
		t.Errorf("dim=0 应回退到 1024，实际 %d", got)
	}
}

// TestTokenize 验证分词策略：中英混排 + 只保留中文 bigram
func TestTokenize(t *testing.T) {
	got := tokenize("使用 Milvus 3.0.2 存储向量")
	have := make(map[string]bool, len(got))
	for _, g := range got {
		have[g] = true
	}
	for _, n := range []string{"Milvus", "3", "0", "2", "使用", "存储", "向量"} {
		if !have[n] {
			t.Errorf("缺少 token %q，实际=%v", n, got)
		}
	}
	// ★ 单字必须不出现：它们既冗余又稀释信号（见 tokenize 注释）
	for _, g := range got {
		if len([]rune(g)) == 1 && !isASCIIDigit(g) {
			t.Errorf("不应产生中文单字 token %q，实际=%v", g, got)
		}
	}
}

func isASCIIDigit(s string) bool {
	r := []rune(s)
	return len(r) == 1 && r[0] >= '0' && r[0] <= '9'
}
