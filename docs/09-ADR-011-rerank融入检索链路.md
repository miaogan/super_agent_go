# ADR-011 · 重排融入检索链路：外扩前打分、失败降级但不静默

| 项目 | 内容 |
|---|---|
| **状态** | ✅ 已采纳 |
| **日期** | 2026-10-09 |
| **相关代码** | `rag_service/internal/retrieve/engine.go`、`llm_service/internal/service/llm.go`、`idl/llmv1/llm.proto` |
| **前置决策** | ADR-008（mock 阈值解耦）、ADR-003（Provider 能力如实声明）、ADR-010（BaseURL 归一化） |

---

## 背景

重排（rerank）能力在 Provider 层已经完整可用：

- `provider.Provider` 接口新增 `Rerank()`，三个实现（rawhttp / chain / mockprovider）全部就绪
- 阿里云百炼 `qwen3.7-text-rerank` 实测通过（`make rerank-probe`，176ms，分数 0.9984 / 0.4239 / 0.2390）
- 25 个单元测试覆盖路径推导、响应解析、越界防护、降级语义

但 `rag_service` 的检索链路**用不上它** —— 缺的是 RPC 声明本身。检索流程仍是纯向量 topK：

```
embedding → Milvus 检索 → 回 PG 取正文 → 上下文外扩 → 拼 Prompt
```

向量召回的固有问题：双塔模型把 query 和 document 各自编码成向量再算余弦，**query 与 doc 之间没有交叉注意力**，语义匹配能力有限。常见表现是「关键词都对、但答非所问」——召回了字面相似的切片，却漏掉真正相关的那条。重排用一个 cross-encoder 逐对计算相关性，正好补上这块。

---

## 决策

### 一、链路形态：重排在「外扩之前」打分，外扩只对最终 topN 做

原链路里，上下文外扩和取正文是绑在一起的（`hydrate`）。重排要插进来，必须先拆开：

```
embedding → Milvus 召回 candidateK(20) → 回 PG 取原文
          → Rerank 20 条 → 阈值过滤 → 取 topN(5)
          → 上下文外扩（只对这 5 条）→ 拼 Prompt
```

**为什么不先外扩再重排：**

| 维度 | 外扩前打分（采纳） | 外扩后打分 |
|---|---|---|
| 请求体大小 | 20 × 800 字 | 20 × 2400 字（window=1 时 ×3） |
| 语义纯度 | cross-encoder 直接比对 query↔切片 | 混入邻片，稀释目标切片的信号 |
| `Neighbors` 调用 | ≤5 次（只对入选结果） | 20 次（对全部候选） |

核心判断：**cross-encoder 打分本质是 query↔切片对**。把相邻切片拼进来，模型看到的是一个混杂了邻近语义的文本块，「这条切片到底和查询有多相关」这个信号被稀释。而外扩的目的本就是「让送进 Prompt 的上下文语义完整」—— 那是重排之后的事。

职责也因此分离了：「选哪几条」由重排决定，「每条补多少上下文」由外扩决定。

### 二、召回数量按 `RerankTopK`，不再 `+ window*2`

```go
func (e *Engine) candidateK(topK int) int {
	if e.rerankOn() && e.cfg.RerankTopK > topK {
		return e.cfg.RerankTopK
	}
	return topK
}
```

若召回数与 topK 相同，重排只能重新排序、无法替换 —— 等于白花一次调用。

原先的 `topK + window*2` 是为「外扩后仍满足 topK」服务的，外扩后置后这项损耗消失，故去掉。多召回的部分现在纯粹用于抵消 PG 中切片缺失被跳过的损耗。

### 三、重排阈值独立于向量阈值，且默认开启

`rerank_score_threshold`（0.3）与 `default_score_threshold`（0.3）量纲不同：

- 向量余弦相似度（真实 embedding 生产实测 0.7+ 为强相关）
- rerank 模型自己的 0–1 相关性分（实测强相关 0.998、勉强 0.42、无关 0.24）

共用一个阈值是常见错误来源：拿 0.3 去卡 rerank 分数会把大量中等相关的文档放进来，反而变差。

`rerank_enabled` 默认 **true**。它能安全默认开启的前提是决策四 —— 失败自动降级。开着它最坏情况就是退化成不开的效果；关掉它则一定拿不到重排收益。

### 四、失败降级，但强制透出标记，且不判检索失败

**这是本决策最重要的部分。** 重排有三种结果，都不是「失败」：

| 情况 | 命中处理 | 检索结果 | `degraded` |
|---|---|---|---|
| 成功 | 按重排分降序取 topN | 重排后结果 | false |
| **能力缺失**（未配置模型 / 缺 `rerank_path`） | 退回候选原序 | 全部候选截 topK | **true** + `rerank_unsupported` |
| **调用失败**（超时 / Provider 报错） | 退回候选原序 | 全部候选截 topK | **true** + `rerank_timeout` / `rerank_failed` |
| 成功但全部低于阈值 | 返回空 | 空 | false，但写明 `rerank_rejected_all` |

三条设计约束：

1. **不返回 error**。重排是增强环节，它失败不该让整次检索失败 —— 那样会把一个可选功能的故障放大成整个 RAG 链路不可用。
2. **必须置 `degraded`**。沿用项目「绝不静默降级」原则：用户必须知道这次回答可能没经过重排、精度打折。
3. **「全部低于阈值」不是降级**。那是模型的正常业务判断（「这批候选全都不相关」），标成技术故障会误导排查。这是 `rerankOutcome` 用结构体而非 `(results, error)` 表达的原因：

```go
type rerankOutcome struct {
    picked        []candidate   // 空 = 全部被阈值拦掉（业务结论）
    degradeReason string        // 非空 = 能力缺失/调用失败（技术降级）
}
```

用二元返回 `(results, degradeReason)` 会把「空结果」和「降级」混为一谈 —— 这正是开发过程中真实踩到的一个 bug，由 `TestRerankRejectedAllIsNotDegraded` 抓到。

### 五、keywordFallback 路径不做重排

向量库不可用时的关键词降级路径**明确不做重排**，三条理由：

1. 该路径已 `Degraded=true`，再叠一层重排降级会让原因混浊，排障时无法定位根因
2. 关键词召回本身按 `ts_rank` 排序、候选仅 `topK` 条，重排的收益（从更多候选里挑）几乎不存在
3. `Stages` 里不加 `rerank` 条目 —— 如实反映「未执行」，而不是写一条误导性记录

该结论已写入代码注释，避免后人误加。

### 六、mock 模式阈值必须下调（与 ADR-008 同构）

`mockprovider.Rerank` 用词袋余弦模拟重排，其相似度上限受数学限制约 **0.25**；而生产阈值 0.3 是为真实 rerank 模型标定的。

若直接套用：**mock 模式下重排恒为 0 命中**，检索链路在冒烟测试里形同虚设 —— 而 CI 全绿，看起来一切正常。

解法与 ADR-008 完全同构，不新增配置项：

```go
func (e *Engine) rerankThreshold() float64 {
    t := e.cfg.RerankScoreThreshold
    if e.mockMode && e.cfg.MockScoreThreshold > 0 && t > e.cfg.MockScoreThreshold {
        return e.cfg.MockScoreThreshold
    }
    return t
}
```

冒烟脚本新增两条断言锁死这一点：

```bash
assert_contains "$BODY" '"stage":"rerank"' "调试响应含 rerank 阶段（重排已真实执行）"
assert_contains "$BODY" 'RAG' "检索命中内容（证明重排后仍有结果，未被阈值全拦）"
```

### 七、`Hit.Score` 覆盖为重排分

重排成功后，`Hit.Score` 从向量余弦改为 rerank 相关性分。理由：调用方按「最终排序依据」判断置信度，而重排才是真正的决策依据。向量分在 Debug 的 `milvus_search` 阶段明细里仍可追溯。

⚠️ 两者量纲不同（余弦 vs 0–1 相关性分），混用会让调用方的阈值判断失真。

---

## 后果

### 正向

- 检索排序质量显著提升：向量召回负责「不漏」，重排负责「选准」
- 重排失败不损失可用性：退回向量序 + 明确标注，用户与排障都不受影响
- 默认开启即可获得收益，无需部署时额外操作

### 代价

- 每次检索多一次外部调用（约 180ms + 少量费用，按次计费的部分厂商成本需核算）
- `hydrate` 拆成 `buildCandidates` + `expandContext` 两个函数，代码量增加
- 新增内部类型 `candidate`（携带 `seq`）—— 因为 `ragv1.Hit` 只有 `PageNo` 没有 `seq`，外扩需要它

### 需要注意

- **启用重排需同时满足两个条件**：`llm.models` 里 rerank 模型 `status: enabled`，**且**填了 `rerank_path`。后者容易漏 —— `rerank_path` 留空时 rawhttp Provider 返回 `ErrUnsupported` 而非拼一个必然 400 的 URL（实测拿模型名当路径段拼 URL，服务端恒返回 `400 url error`）
- `candidateK` 必须 > `topK`，否则重排无替换空间
- 重排 RPC 复用 `milvus.timeout`（默认 10s）作为超时，未单配

---

## 相关文档

- `configs/config.example.yaml` `rag:` 段 —— 4 个配置项及注释
- `README.md`「模型接入（阿里云百炼 compatible-mode）」—— 端点实测记录
- `docs/05-部署运维手册.md` —— 环境变量与排障条目