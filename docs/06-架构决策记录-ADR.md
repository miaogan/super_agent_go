# 架构决策记录（ADR）

本文档记录 Aegis 平台的关键架构决策，包含**背景、备选方案、决策依据与后果**。目的是让后续加入的团队成员理解决策背后的权衡，避免重复讨论。

> **维护约定**：每个决策一经采纳即编号存档，**不修改历史记录**。若决策变更，新增一条 ADR 并将旧条目状态置为「已废弃」，注明替代者。

---

## ADR-001 · 服务按业务能力垂直拆分（5 个服务）

| 项 | 内容 |
|---|---|
| **状态** | ✅ 已采纳 |
| **日期** | 2026-10-08 |
| **决策者** | 架构组 |

### 背景

平台需要承载认证、用户、Agent 编排、模型接入四类能力。核心问题是：拆成几个服务？边界画在哪里？

### 备选方案

| 方案 | 描述 |
|---|---|
| A. 单体应用 | 一个服务包含全部逻辑 |
| B. 按技术分层拆分 | 网关层 / 业务层 / 数据层分别成服务 |
| **C. 按业务能力垂直拆分** | **gateway / auth / user / agent / llm 五服务**（采纳） |
| D. 按读写分离拆分 | 读写各成服务 |

### 决策

采用方案 C，按**业务能力**垂直拆分，每个服务拥有独立的职责边界与数据所有权。

### 依据

| 维度 | 分析 |
|---|---|
| **团队规模** | 10 人以上，具备并行维护多服务的人力 |
| **平台定位** | 面向多业务方接入的平台，需要独立的治理与计量能力 |
| **变更频率** | 认证逻辑（合规驱动）、模型接入（厂商驱动）、Agent 能力（产品驱动）三者演进节奏完全不同，耦合会导致相互阻塞 |
| **故障隔离** | 模型厂商故障不应影响用户登录；认证服务抖动不应阻断 Agent 对话 |
| **技术异构** | `llm_service` 需要更强的重试/熔断策略与不同的超时基线，与 CRUD 类服务差异大 |
| **独立扩缩容** | 对话高峰期 `agent_service` / `llm_service` 需扩容，而 `auth_service` 无需同比例扩容 |

### 后果

**正面**

- 故障域隔离，单服务异常不扩散
- 各服务可独立选型、独立部署、独立扩缩容
- 服务边界即团队边界，便于并行开发

**负面（需主动管理）**

- 运维复杂度上升：5 个 Deployment + 5 套监控 + 5 条 CI 流水线
- 跨服务调用需要处理分布式事务与一致性问题
- 本地开发需依赖 Docker Compose 拉起中间件
- 端到端问题定位需依赖全链路追踪

**缓解措施**

- 统一脚手架与 CI 模板，降低单服务维护成本
- 全链路 TraceID 贯穿，配合统一日志规范
- 提供 `docker-compose.yaml` 一键拉起全套依赖
- 关键链路补集成测试

### 相关决策

- [ADR-002](#adr-002--llm_service-保持独立不合并进-agent_service) —— 本决策的延伸：模型接入能力必须独立成服务

---

## ADR-002 · llm_service 保持独立，不合并进 agent_service

| 项 | 内容 |
|---|---|
| **状态** | ✅ 已采纳 |
| **日期** | 2026-10-08 |
| **决策者** | 架构组 |

### 背景

在服务拆分讨论中，有观点提出：将 `llm_service` 取消，模型调用能力全部交由 `agent_service` 处理，理由是减少服务数量、降低运维成本、缩短调用链路。

### 备选方案

| 方案 | 描述 | 结论 |
|---|---|---|
| A. 物理合并，逻辑保留 | 合并为 4 服务，但 llm 相关逻辑独立成 package | 不采纳（当前阶段） |
| B. 彻底合并 | 模型调用直接写在 agent_service 业务逻辑中 | ❌ 明确否决 |
| **C. 保持独立** | **两服务物理分离** | **✅ 采纳** |

### 决策

`llm_service` **保持独立**，不合并进 `agent_service`。

### 依据

#### 1. llm_service 承载的是「网关职责」而非「Agent 逻辑」

拆解其五项职责后可见，**没有一项属于业务编排**：

| 职责 | 性质 | 归属判断 |
|---|---|---|
| 模型路由 + 故障降级 | 资源调度 | ✅ 网关职责 |
| Token 计量 + 成本核算 | 平台账本 | ✅ 网关职责 |
| 厂商适配（Provider 层） | 协议收敛 | ✅ 网关职责 |
| 配额校验 | 资源治理 | ✅ 网关职责 |
| 模型调用转发 | 纯转发 | ❌ 可合并，但无实益 |

`llm_service` 本质是**站在模型前面的第二层网关**，与 `gateway` 同类（后者站在 HTTP 前面）。既然 `gateway` 独立成立，`llm_service` 的独立性同样成立。

#### 2. 与平台定位冲突

本平台定位为**多业务方接入的 AI 能力平台**。平台化前提是**能力可被独立复用与独立治理**：

- 若合并，第二方业务想直接调用模型能力，就必须依赖 `agent_service` 的完整业务逻辑
- 平台的成本分摊、配额管控、SLA 统计失去统一数据源

#### 3. Token 计量会退化为「自记账」

计量下沉后，每个使用方各自计算，导致：

- 跨应用 / 跨业务线成本无法汇总
- 报表口径不一致
- 后续新增 Agent 场景需重复实现计量逻辑

#### 4. 熔断与降级链失效

模型降级依赖**共享的熔断状态**（存于 Redis）。合并后：

- `agent_service` 多副本各自维护熔断状态，A 副本熔断后 B 副本仍向故障厂商发送流量
- 降级逻辑从「一次调用内的优先级链」变为「业务代码里的分支判断」，可维护性下降
- 若为共享状态引入 Redis 交互，等于把 `llm_service` 的核心逻辑搬进 `agent_service`——**复杂度未降低，只是转移了位置**

#### 5. 合并的收益不成立

| 声称收益 | 实际评估 |
|---|---|
| 减少网络跳数，降低延迟 | **不成立**。省下的 0.5–2ms 被 LLM 推理的数百 ms 完全淹没 |
| 降低运维成本 | 成立，但 10 人以上团队可承担 |
| 减少代码生成与 IDL 维护 | 成立，但为一次性成本 |
| 调试链路更短 | 成立，通过本地联调工具可缓解 |

**关键判断：合并的经典理由（减少网络跳数）在 LLM 场景下不成立**，不应作为决策依据。

### 后果

**正面**

- 模型能力可被独立复用，支撑平台化定位
- 计量与成本核算有统一数据源
- 熔断降级逻辑集中，状态共享可靠
- 模型层与业务层独立演进，互不阻塞

**负面**

- 多一跳 RPC（约 0.5–2ms，对 LLM 场景无感）
- 多一个服务的运维成本

**强制约束（不可妥协）**

无论未来是否调整物理拓扑，以下约束必须保持：

```
pkg/llm/provider  ← 必须独立成包，与任何服务解耦
   ├── provider.go         自研稳定接口
   ├── eino_provider.go    Eino 实现
   └── rawhttp_provider.go 裸 HTTP 兜底
```

**只要 Adapter 层独立，物理拓扑随时可调整**：现在两服务共享该包；若合并则为内部 package；若需重新拆分则直接抽出，零重构成本。

### 重新评估的触发条件

若出现以下情况，应重新评估本决策：

| 条件 | 说明 |
|---|---|
| 团队缩减至 3 人以下 | 运维负担成为主要矛盾 |
| 长期仅服务单一业务方 | 平台属性消失，无需独立网关 |
| 模型只用 1 家且无切换计划 | 路由与降级能力闲置 |
| 不对外提供模型能力 | 无需独立网关边界 |
| `agent_service` 成为明显瓶颈 | 需重新评估拆分粒度（此时更应拆分而非合并） |

### 相关决策

- [ADR-001](#adr-001--服务按业务能力垂直拆分5-个服务) —— 本决策的上位决策
- [ADR-003](#adr-003--eino-锁定稳定线-v0921adapter-隔离) —— Provider 层实现约束

---

## ADR-003 · Eino 锁定稳定线 v0.9.21，Adapter 隔离

| 项 | 内容 |
|---|---|
| **状态** | ✅ 已采纳 |
| **日期** | 2026-10-08 |
| **决策者** | 架构组 |

### 背景

Eino 是 Go 生态最成熟的 LLM 应用框架，但处于 0.x 快速迭代阶段，且存在破坏性更新风险。需要决定：使用哪个版本？如何控制升级风险？

### 版本现状（决策时）

| 线路 | 最新版本 | 发布时间 | 性质 |
|---|---|---|---|
| 稳定线 | **v0.9.21** | 2026-09-23 | GitHub 标记 Latest |
| alpha 线 | v0.10.0-alpha.35 | 2026-09-20 | 官方标注 Pre-release |

v0.10.0-alpha 平均 **2–3 天一个版本**，官方明确提示仍可能发生 Breaking Changes。

### 决策

**生产依赖锁定 `v0.9.21` 稳定线，不使用 alpha 版本**，并通过 Adapter 层隔离。

### 依据

#### 1. 「稳定版」不等于「API 冻结」

Eino 0.x 阶段，v0.9 作为稳定线同样经历大规模 API 废弃：

| 已废弃 API | 迁移方向 |
|---|---|
| `Agent Transfer`（`SetSubAgents` / `AgentWithOptions` 等） | `ChatModelAgent + AgentTool` 或 `DeepAgent` |
| `Workflow Agent`（`NewSequentialAgent` / `NewParallelAgent` / `NewLoopAgent`） | 同上 |
| `supervisor.New` / `supervisor.Config` | 同上 |
| `AgentMiddleware` + `ChatModelAgentConfig.Middlewares` | 接口式 `Handlers []ChatModelAgentMiddleware` |
| `summarization.SummarizeMessages` / `SummarizeOutput` | `summarization.New` / `TypedMiddleware.Summarize` |

**因此任何 Eino 版本都必须配合适配层使用**，而非寄望于某个版本"足够稳定"。

#### 2. alpha 版本号存在语义陷阱

`v0.10.0-alpha.35` 与未来的 `v0.10.0` 正式版**不属于同一版本序列**。在实验分支使用 alpha 后，转正时仍需重新适配，**不存在平滑升级路径**。

#### 3. 本项目仅使用 Eino 最薄的一层

当前需求仅涉及 `ChatModel` 抽象与 `Generate` / `Stream` 调用，**未触及 ADK、Compose 等高变动部分**。该层抽象长期稳定，隔离成本极低。

### 实施方案

```
app/agent_service（业务层）        ← 零 Eino import
        │
        ▼
pkg/llm/provider.Provider          ← 自研稳定接口
        │
        ├── einoProvider           ← Eino 仅出现在此 1 个文件（功能完整，默认启用）
        └── rawHTTPProvider        ← 裸 HTTP 薄客户端（能力子集，降级时启用）
```

**四条规约**

1. 生产锁 `v0.9.21`，用 `replace` 防止间接依赖拖入 alpha
2. 业务代码禁止直接 import Eino，类型不得跨越 Provider 边界
3. `einoProvider` 与 `rawHTTPProvider` 共用一套契约测试
4. 升级必须：读 migration notes → 契约测试全绿 → 端到端回归（同步/流式/工具调用）→ 灰度发布

#### 兜底实现的设计取舍（`rawHTTPProvider`）

**要解决的问题**：本 ADR 的核心承诺是"业务侧零 Eino 依赖、Eino 只是可替换的实现之一"。若只有 `einoProvider` 一个实现，该承诺是**空的**——Eino 出问题时没有第二条腿。但兜底层**不必是"第二个完整框架"**。

**决策：兜底层实现为「薄客户端」，覆盖最小可用子集，而非对等复制品。**

| 能力 | `einoProvider` | `rawHTTPProvider` |
|---|---|---|
| `Chat`（非流式） | ✅ | ✅ |
| `ChatStream`（纯文本 delta） | ✅ | ✅ |
| `Embed` | ✅ | ✅ |
| `ToolCall` | ✅ | ❌ 不支持 |
| `Reasoning`（思维链） | ✅ | ❌ 不支持 |
| 多模态输入 | ✅ | ❌ 不支持 |
| 第三方依赖 | Eino + eino-ext | **仅标准库 + Sonic** |

**能力差异必须显式声明，而非运行时崩溃。** 接口提供 `Capabilities()`，上层在兜底模式下**明确降级**（如禁用工具调用、关闭多模态输入），并可将降级状态透出到响应中：

```go
// pkg/llm/provider/provider.go
type Provider interface {
    Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
    ChatStream(ctx context.Context, req *ChatRequest) (StreamReader, error)
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Capabilities() Capabilities
    Name() string
}

type Capabilities struct {
    ToolCall   bool
    Reasoning  bool
    Multimodal bool
    Embeddings bool
}
```

**为什么兜底层不引入第三方 SDK（如 openai-go）**：本决策的立足点是"**零第三方依赖的最后一道防线**"。若兜底层自身也依赖一个第三方 SDK，则该防线的成色打折——供应链问题、SDK 自身破坏性更新都会同时击穿两层。OpenAI 兼容接口的实质是 `POST /v1/chat/completions`（JSON）+ SSE 流式，用标准库 + 已有的 Sonic 实现成本约 **150 行**（其中大部分是 SSE 解析，写一次即稳定）。

**运行时开关**：Nacos 配置 `llm.provider: eino | rawhttp`，支持运行时切换，无需重新发版。

### 后果

**正面**

- Eino 破坏性升级的影响面被压缩至单个文件
- 可随时回退到零框架依赖方案
- 双实现可作 A/B 对比，验证性能与稳定性
- 业务逻辑测试无需依赖真实模型

**负面**

- 需要维护一层类型转换代码（约数十行）
- 无法直接使用 Eino 的高层 API（如 ADK 的便利方法）
- 双实现需同步维护；兜底层虽为能力子集（约 150 行），但仍需随接口演进而更新

### v0.10 跟踪项（不采用，仅关注）

| 能力 | 说明 |
|---|---|
| Durable ADK Runtime | Agent 运行时状态持久化 |
| Session / SessionStore | 跨 Turn 会话状态管理 |
| Background Execution | 后台任务执行 |
| TurnLoop + Checkpoint 整合 | 轮次循环与检查点统一 |
| Memory Middleware | 长期记忆中间件 |

> **概念分离提醒**：`Session`（跨 Turn 会话状态）、`Checkpoint`（被中断的运行状态）、`Memory`（长期可复用信息）是三个不同概念，**不应混存到一张表**。本项目数据模型设计已遵循该原则。

### 相关决策

- [ADR-002](#adr-002--llm_service-保持独立不合并进-agent_service) —— Provider 层的物理归属

---

## ADR-004 · 统一 Go 1.26 + Sonic ≥ v1.15.0

| 项 | 内容 |
|---|---|
| **状态** | ✅ 已采纳 |
| **日期** | 2026-10-08 |
| **决策者** | 架构组 |

### 背景

需要选定 Go 版本，并决定是否引入 Sonic（JIT + SIMD 加速的 JSON 库，Hertz 默认集成）。Sonic 的兼容性与 Go 版本强相关。

### 决策

**全环境统一 Go 1.26.x，Sonic 锁定 ≥ v1.15.0（推荐 v1.15.4）。**

### 依据

#### 1. Go 1.26 是 Sonic 原生加速支持的最后一个版本

Sonic 官方兼容矩阵：

```
amd64 CPU:  go1.17 ~ go1.26
arm64 CPU:  go1.20 ~ go1.26
```

升级至 Go 1.27+ 后，Sonic 将打印警告并静默回退 `encoding/json`，JSON 性能退回基线。

#### 2. ⚠️ Sonic 必须 ≥ v1.15.0，否则编译失败

Go 1.26 将内置 map 重写为 Swiss Table，runtime 侧 `mapiterinit` / `mapiternext` 签名与迭代器类型完全改变。Sonic 通过 `//go:linkname` 绑定这些未导出符号：

| Sonic 版本 | Go 1.26 表现 |
|---|---|
| ≤ v1.14.1 | ❌ **编译失败**：`undefined: GoMapIterator` |
| **≥ v1.15.0** | ✅ 正常编译，原生加速可用 |

因 Go 包级编译模型，依赖图中存在 Sonic 时整个 `go build` 失败，`go mod tidy` 无法剔除。

#### 3. Go 1.26 带来的实质收益

| 特性 | 价值 |
|---|---|
| Green Tea GC 默认开启 | GC 开销降 10–40%，利好高频小对象场景（消息、上下文） |
| cgo 调用开销降约 30% | Sonic 的 C 核心函数调用更快 |
| 切片底层存储更多栈分配 | 减少堆分配 |
| `crypto/tls` 抗量子混合密钥交换 | 传输层安全增强 |

### 后果

**正面**

- 获得 Sonic 原生加速（序列化快 2–3 倍，反序列化快 3–4 倍）
- 获得 Go 1.26 的性能与安全收益

**负面 / 约束**

- **技术栈被锁定在 Go 1.26**，无法跟进 1.27+ 新特性
- Sonic 版本窗口未来会收窄，需关注替代方案
- 需在 CI 与启动时双重校验，防止误用 `latest` 镜像或不受支持的架构

### 强制约束

| 项 | 约定 |
|---|---|
| `go.work` / `go.mod` | `go 1.26` + `toolchain go1.26.x` |
| CI 构建镜像 | 固定 `golang:1.26-alpine`，**禁止 `latest`** |
| Sonic 版本 | `v1.15.4`，配 `replace` 防版本漂移 |
| 构建标签 | **禁用** `-tags stdjson` 与 `-tags compat` |
| 启动自检 | 断言 `sonic.APIKind == sonic.UseSonicJSON` |
| 应急退路 | 若必须用 1.27+，需替换为标准库或评估 `sonic-rs` |

### 相关决策

- [ADR-003](#adr-003--eino-锁定稳定线-v0921adapter-隔离) —— 同样采用「锁版本 + 运行时自检」的防漂移思路

---

## ADR-005 · 关系型数据库选用 PostgreSQL 18（替代 MySQL 8.0）

| 项 | 内容 |
|---|---|
| **状态** | ✅ 已采纳 |
| **日期** | 2026-10-08 |
| **决策者** | 架构组 |
| **替代** | 原方案「MySQL 8.0」 |

### 背景

平台原设计使用 MySQL 8.0 存储用户、会话、消息、计量等数据。随着 RAG 能力引入与数据量增长，需要重新评估关系型数据库选型。

关键驱动因素：
1. 引入 RAG 后，**JSON 结构化数据的存储与查询需求显著增加**（文档 metadata、chunk 元数据、工具配置、审计详情）
2. `messages` / `token_usage` / `audit_logs` 年增量达数千万行，**分区能力**成为硬需求
3. 团队对「减少基础设施种类」有明确诉求

### 备选方案

| 方案 | 描述 |
|---|---|
| A. 保持 MySQL 8.0 | 继续使用原方案，稳态无风险 |
| B. 升级到 MySQL 8.4 / 9.x | 跟进 MySQL 新版本 |
| **C. 迁移到 PostgreSQL 18** | **采纳** |
| D. 分布式数据库（TiDB / OceanBase） | 为未来超大流量预留 |

### 决策

采用**方案 C：PostgreSQL 18**。

### 依据

| 维度 | 分析 |
|---|---|
| **JSON 支持** | PG 的 **`JSONB`** 是二进制存储，支持 **GIN 索引**与 `@>` `->` `?` 等丰富操作符。MySQL 的 `JSON` 是文本存储，多值索引能力受限且语法复杂。RAG 场景大量元数据查询受益明显 |
| **数组类型** | PG 原生支持 **`BIGINT[]`** + GIN 索引，可简洁表达「Agent 挂载多个知识库」（`kb_ids @> ARRAY[5]`），MySQL 必须建中间表 |
| **原生分区** | PG 的声明式分区（`PARTITION BY RANGE`）配合 `pg_partman` 可自动滚动；`DETACH PARTITION` 是**元数据级操作**，归档瞬间完成。MySQL 分区维护更繁琐 |
| **部分索引** | PG 支持 `WHERE deleted_at IS NULL` 的部分索引，软删除场景下索引体积显著更小 |
| **索引方向** | PG 索引可指定 `DESC`，`(uid, created_at DESC)` 让会话列表排序直接走索引 |
| **生态趋势** | 2026 年 PG 在 AI 基础设施领域已成为事实标准，与向量方案（pgvector）天然亲和，社区活跃度领先 |
| **运维成熟度** | CloudNativePG / Zalando Operator 成熟；云厂商 RDS 支持完善 |
| **DDL 事务性** | PG 的 DDL 支持事务，变更失败可自动回滚；MySQL 的 DDL 会隐式提交，回滚需手工处理 |
| **`uuidv7()`** | PG 18 内置时间戳可排序 UUID 生成函数，对分布式 ID 场景有额外价值 |

### 后果

**正面**

- RAG 元数据查询能力大幅提升（JSONB + GIN）
- 分区、部分索引、数组类型等能力直接简化数据模型设计
- 与未来向量方案（pgvector）保持同一生态，避免技术栈分裂
- DDL 可事务回滚，数据库变更风险降低

**负面 / 约束**

- **语法不兼容 MySQL**：全部 DDL 需重写（`AUTO_INCREMENT` → `GENERATED ... AS IDENTITY`、`TINYINT` → `SMALLINT`、`JSON` → `JSONB` 等），映射表见 [04-数据库设计.md §1.1](./04-数据库设计.md)
- **`ON UPDATE CURRENT_TIMESTAMP` 无等价物**：必须用触发器维护 `updated_at`，批量更新时需注意触发器开销
- **分区表约束**：分区键必须包含在主键内，`messages` 主键从 `msg_id` 变为 `(msg_id, created_at)`
- **进程模型**：PG 每连接一个进程，内存开销高于 MySQL 线程模型。**必须经 PgBouncer 复用连接**，否则高并发下连接数先于 CPU 成为瓶颈
- **MVCC 膨胀**：UPDATE 产生死元组，依赖 autovacuum 回收，**必须监控 `n_dead_tup`** 并对高频更新表单独调优
- 团队若更熟悉 MySQL，需一定的学习成本

**缓解措施**

| 风险 | 措施 |
|---|---|
| 连接数瓶颈 | 强制经 PgBouncer（transaction 模式），应用侧连接池上限 20 |
| 表膨胀 | 监控 `n_dead_tup`，对 `sessions` / `quotas` 单独设置 autovacuum 阈值 |
| 分区维护遗漏 | 引入 `pg_partman` + `pg_cron` 自动滚动分区 |
| 团队学习成本 | 04 文档提供完整语法映射速查表；07 运维要点章节覆盖关键差异 |
| 迁移风险 | **全新项目从零建库**，不涉及存量数据迁移 |

### 相关决策

- [ADR-006](#adr-006--向量库选用-milvus-而非-pgvector) —— 向量库选型独立评估（最终未选 pgvector，但 PG 仍是元数据主库）
- [ADR-007](#adr-007--rag_service-独立部署不并入-agent_service) —— RAG 服务拆分

---

## ADR-006 · 向量库选用 Milvus（而非 pgvector）

| 项 | 内容 |
|---|---|
| **状态** | ✅ 已采纳 |
| **日期** | 2026-10-08 |
| **决策者** | 架构组 |

### 背景

引入 RAG 需要向量存储与 ANN 检索能力。核心问题是：用**与主库统一的 pgvector**，还是**独立的专业向量数据库**？

### 备选方案

| 方案 | 描述 |
|---|---|
| A. pgvector | PostgreSQL 扩展，与主库同实例 |
| **B. Milvus 3.0.x** | **专业向量数据库（采纳）** |
| C. Elasticsearch / OpenSearch | 全文检索为主，兼有向量能力 |
| D. 云托管向量服务 | 依赖厂商，数据出域 |

### 决策

采用**方案 B：Milvus 3.0.2 服务端 + 自研 client/v3 适配层**。

- 服务端镜像：`milvusdb/milvus:v3.0.2`
- Go 客户端：`github.com/milvus-io/milvus/client/v3 **v3.0.0**`

> ⚠️ **版本号订正（2026-10-09，实现时核实）**
>
> 本ADR 初版写的是「客户端 `client/v3 v3.0.2`」。**该版本不存在**：
>
> ```
> client/v3 @v/list → v3.0.0-beta, v3.0.0     ← 没有 v3.0.2
> client/v3 @latest → v3.0.0 (2026-09-11)
> ```
>
> `v3.0.2` 是**服务端 `milvusdb/milvus` 的补丁版本号**，不是客户端版本。
> 照抄会得到 `no matching versions for query ... v3.0.2`，
> 而报错信息**不会提示「你想写的是服务端版本」** —— 很容易误判为网络或代理问题。
>
> 客户端与服务端是两条独立的版本线，不必对齐号。
>
> 📌 **修订记录（2026-10-08）**：本 ADR 初版锁定 **Milvus 2.6.x**，理由是"3.0 刚 GA、生态未沉淀、新特性对本项目无收益"。经复核后**修订为 3.0.2**，因为最初判断中的两条前提已不成立——① 3.0 已产出可用补丁线（3.0.0 → 3.0.1 → 3.0.2），Go 客户端已转正式 GA；② 3.0.2 修复了 filtered search / group-by / 索引构建的热路径竞争与 rolling upgrade 混合版本一致性问题，具备生产可用性。同时**明确接受一项代价**：放弃 eino-ext 官方 `milvus2` 组件，自研 `pkg/vectordb/milvus3` 适配层。详见"依据"与"后果"。

### 依据

| 维度 | 分析 |
|---|---|
| **扩展上限** | pgvector 在千万级以下表现良好（实测 300 万条 1024 维：P99 180ms、QPS 210），但千万级以上需认真评估。**Milvus 从设计上支持十亿级**，一次选型免去中期迁移 |
| **版本可用性** | 3.0 已有三级补丁线（2026-07-29 GA → 09-09 3.0.1 → 09-18 3.0.2），Go 客户端 `client/v3.0.0` 已转正式 GA SemVer，proto 契约稳定。3.0.2 集中修复了 search/query 热路径竞争与 streaming/compaction 稳定性 |
| **独立扩缩容** | Milvus 可独立于主库扩容（加 query node），不会因向量检索压力拖累业务库的 OLTP 性能 |
| **资源隔离** | 向量检索是内存密集型（索引常驻），与业务库混部会争抢 `shared_buffers` 与 OS page cache，影响业务查询稳定性 |
| **分区能力** | Milvus 支持 `partition_key_field` 按 `kb_id` 物理隔离数据，检索性能与隔离性更好 |
| **专业索引** | 支持 HNSW / IVF / DISKANN / SCANN 等多种索引，可按数据规模与内存预算灵活选择；pgvector 索引类型相对有限 |
| **量化压缩** | Milvus 原生支持 `FLOAT16` / `BFLOAT16` 向量类型与量化，可将向量体积压缩 50–75% |

### 反面考量（为何 pgvector 也很有吸引力）

必须诚实记录 pgvector 的优势，避免后人反复质疑：

| pgvector 优势 | 为何仍不选 |
|---|---|
| 与主库统一，**零新增基础设施** | 代价是向量负载与 OLTP 负载耦合，互相影响 |
| 事务一致（与业务数据同库） | 本项目采用「PG 为内容真相源 + 最终一致 + 补偿任务」方案，不强依赖分布式事务 |
| 运维复杂度极低（3/10） | Milvus 的 etcd + MinIO 确有运维成本，但可接受，且换来扩展上限 |
| 中小规模性能足够 | 平台定位是「支撑 100+ 业务应用、日调用百万级」，需为增长预留空间 |

> **务实路径**：若未来团队规模收缩、或数据量长期停留在百万级以内，Milvus 可降级替换为 pgvector。为此，rag_service 内的 `store.VectorStore` 接口做了抽象，替换影响面限制在单个实现文件中。

### 后果

**正面**

- 扩展上限高，十亿级规模无需重新选型
- 使用 3.0.x 主线，可跟进官方长期演进方向（2.6 为维护线）
- 向量负载与业务库物理隔离，互不影响
- 索引类型丰富，性能调优空间大

**负面 / 约束**

- **新增三类基础设施**：etcd（元数据）+ MinIO（对象存储）+ Milvus（引擎）
- **etcd 对磁盘极度敏感**：必须 SSD，IOPS > 500、P99 fsync < 10ms，否则频繁选举
- **内存要求高**：索引常驻内存，建议 32 GB 起步
- **安全责任增加**：需处理 CVE-2026-26190（9091 端口认证绕过，CVSS 9.8）；且 **3.0 默认 `authorizationEnabled: false`、默认 root 密码为 `Milvus`**，必须显式加固
- **自研适配层成本**：eino-ext 官方仅提供 `milvus2`（依赖 `client/v2`），与 3.0 的 `client/v3` 类型不互通，需自研 `pkg/vectordb/milvus3`。**这抵消了当初"官方组件"这一加分项**。实际落地规模约 560 行（含20 个单测），**远高于决策时的 300 行估算** —— v3 的 API 面比预期大得多（建集合/索引/加载/一致性/Flush 都要自己接）
- **回滚边界更硬**：3.0 的 Storage V3 / 索引版本 / WAL 语义 / SDK-proto 四条边界中，任一边界被越过即无法靠回滚镜像恢复（见下"缓解措施"）
- **升级路径受限**：官方不支持任意版本直升，不允许切换 MQ 类型，不允许从 rc 直升

**缓解措施**

| 风险 | 措施 |
|---|---|
| etcd 磁盘性能 | 部署文档明确要求 SSD，提供 `fio` 验证方法 |
| CVE-2026-26190 | 锁 3.0.2；9091 绑定 `127.0.0.1`；K8s NetworkPolicy；**显式开启 `authorizationEnabled`**；`etcd.rootPath` 改非默认值 |
| 3.0 默认凭据风险 | 上线前必须修改默认 root 密码 `Milvus`；MinIO 凭据非默认值；9000/9001 不对外暴露。**⚠️ 开启鉴权后 `.env` 必须同步补 `MILVUS_USERNAME`/`MILVUS_PASSWORD`** —— 缺失时报的是 `Unauthenticated`，长得像「连不上」，极易误判为 Milvus 未启动 |
| 自研适配层 | 接口面极窄（`Store` / `Retrieve` 各一个方法）；**持续跟进 eino-ext 是否发布 `milvus3` 官方组件，一旦发布优先替换** |
| **写入后暂时不可检索** | 已通过 `vectordb.Syncer` 在**整篇文档写完后 Flush 一次**解决（实测约 1.2s）。`make milvus-probe` 会验证该行为 |
| 回滚边界 | **保持 `useLoonFFI=false`、不提升索引版本**（回滚窗口敞开）；升级前必须做 etcd snapshot + MinIO 全量备份；服务端/客户端/proto 同窗口对齐 |
| 运维复杂度 | 提供完整 docker-compose 与 K8s StatefulSet 模板 |
| 可用性风险 | **Milvus 故障时 rag_service 降级为纯 LLM 对话**，主链路不受影响 |
| 数据丢失 | **PG 是内容真相源**，最坏情况可从 `chunks` 表全量重建向量 |
| 未来需替换 | `store.VectorStore` 接口抽象，替换影响面限制在 `pkg/vectordb/` 内；必要时可回退到 `client/v2` + eino-ext 官方组件 + 2.6.x |
| 版本升级风险 | 锁 3.0.2；升级前必须备份 etcd + MinIO；禁止跨大版本直接升级 |

### 落地状态（2026-10-09 核对）

**已落地**。`milvus3` 适配层已实现并在真实 Milvus 3.0.2 上验证通过：

| 项 | 状态 |
|---|---|
| `pkg/vectordb` 接口（`Store` / `Chunk` / `SearchRequest`） | ✅ 已定义 |
| `pkg/vectordb/mockstore`（内存实现） | ✅ 已实现，支撑 mock 冒烟 |
| **`pkg/vectordb/milvus3`（真实 client/v3 v3.0.0 适配层）** | ✅ **已实现**（`store.go` / `crud.go` / `health.go` + 20 个单测） |
| `rag_service` 接线 | ✅ 非 mock 模式连接真实 Milvus；**连接失败即启动失败，不静默降级** |
| `docker-compose.yml` 的 Milvus 服务 | ✅ 已对齐 3.0.2 + 鉴权加固 |
| 真实链路探测（`make milvus-probe`） | ✅ 11/11 通过 |

#### 实现中踩到、并已写进代码注释的坑

这些不是"配置没调对"，而是**不查源码就一定会写错**的地方：

| 坑 | 实测事实 | 代价 |
|---|---|---|
| **集合必须 load 才能检索** | 新建集合默认 NotLoad，检索报 `collection not loaded`，而**该错误信息里没有任何「请先 load」的提示** | 极易误判为 schema 问题 |
| **建库必须在连库之后** | `New(DBName="aegis")` → `database not found[database=aegis]`；SDK 在 `New()` 内部就校验库存在，且**是阻塞连接不是懒连接** | 建库代码根本没机会执行 |
| **过滤方法名不统一** | `searchOption.WithFilter` / `queryOption.WithFilter` / **`deleteOption.WithExpr`** | 照记忆写编译不过 |
| **genproto ambiguous import** | 老版本来自**多条互不相干**的依赖链（实测依次 20200526/20210513/20210624/20190819），逐个 exclude 是打地鼠 | 正解是升级到拆分后的 genproto 版本 |
| **`row_count` 有延迟** | 写入后立即查 `Stats` 得 0 是**正常现象**（需等 flush/compaction） | 判断写入成功应看 Search 能否召回 |

#### 一致性：写入后立刻检索不到（已修复）

实测发现 Milvus 默认 `ClBounded` 一致性下，**新写入的数据在落盘前对检索不可见，且完全不报错**：

```
Upsert → 立即 Search      → 召回 0 条（无任何错误）
Upsert → 等 1s → Search   → 正常召回
Upsert → Flush+Await → Search → 立即正常召回（实测耗时约 1.2s）
同一时刻改用 ClStrong 检索  → 正常召回
```

**为什么这个缺陷特别危险**：入库流水线在 `Upsert` 成功后立即把 `vec_status` 标记为「就绪」。
于是用户看到文档状态是「已就绪」，紧接着提问却检索不到内容，**且没有任何错误提示**。

**修复方式**：新增 `vectordb.Syncer` 可选接口（未放进 `Store` 主接口——它是特定后端的概念，
不该迫使 mockstore 这类"写入即可见"的实现写空方法），
`milvus3` 用 `Flush` + `Await` 实现，`rag_service` 在**整篇文档写完后调用一次**。

两个关键取舍：

| 决策 | 理由 |
|---|---|
| **在写路径 Sync，不在读路径改ClStrong** | ClStrong 能解决，但它要求**每次检索**都等写入落盘。检索是高频操作，代价会被 QPS 放大成用户可感知的卡顿；而入库是低频批量操作，一次 1.2s 相对解析 + embedding 耗时可忽略。**低频写路径付一次钱，好过高频读路径反复付** |
| **整篇文档 Sync 一次，不每批都Sync** | 分批 32 条/批，几百 chunk 的文档会有十几批，每批一次就是十几秒纯等待 |

> 💡 顺带修正一个**测试方法上的坑**：最初的探测脚本复用同一集合，
> 于是「召回的到底是本次写入还是上次的残留数据」分不出来，
> 表现为"第一次跑通过、第二次跑检索为空"——
> 看起来像一致性 bug，实际是**测试自身让结果失去了意义**。
> 现已改为探测前用专属 `kb_id` 清场，且断言只可能来自本次写入的主键。

### 重新评估触发条件

出现以下任一情况时，应重新评估本决策：

1. 数据量长期停留在 **100 万 chunk 以内**，且无增长预期
2. 团队规模缩至 **3 人以下**，无力承担 Milvus 运维
3. Milvus 出现在生产环境中**反复造成可用性事故**，且无法通过降级策略化解
4. pgvector 能力出现**质变**（如原生十亿级支持）
5. **3.0 主线出现严重回归**：若 3.0.x 连续出现数据正确性 / 可用性事故，回退路径为「`client/v2` + eino-ext 官方 `milvus2` 组件 + Milvus 2.6.25」，前提是**未启用 Storage V3 且未提升索引版本**（这也是坚持不启用这两项的重要原因）
6. **eino-ext 发布 `milvus3` 官方组件**：届时替换自研适配层，重新评估结论（预期为正向，无需变更数据库选型）

### 相关决策

- [ADR-005](#adr-005--关系型数据库选用-postgresql-18替代-mysql-80) —— PG 18 作为元数据主库
- [ADR-007](#adr-007--rag_service-独立部署不并入-agent_service) —— RAG 服务拆分
- [ADR-003](#adr-003--eino-锁定稳定线-v0921adapter-隔离) —— 同样采用「接口隔离 + 多实现」的防漂移思路

---

## ADR-007 · rag_service 独立部署，不并入 agent_service

| 项 | 内容 |
|---|---|
| **状态** | ✅ 已采纳 |
| **日期** | 2026-10-08 |
| **决策者** | 架构组 |

### 背景

平台新增 RAG 能力。核心问题是：RAG 逻辑做成**独立服务**，还是**并入 `agent_service`**？

这与 [ADR-002](#adr-002--llm_service-保持独立不合并进-agent_service) 是同类问题（「某能力是否值得独立成服务」），但 RAG 的特征与 LLM 接入不同，需独立论证。

### 备选方案

| 方案 | 描述 |
|---|---|
| A. 并入 agent_service | RAG 作为 Agent 的一个能力模块 |
| **B. 独立 rag_service** | **采纳** |
| C. 并入 llm_service | 认为 RAG 属于「模型能力」 |

### 决策

采用**方案 B：独立 `rag_service`**（端口 8885）。

### 依据

| 维度 | 分析 |
|---|---|
| **负载特征截然不同** | `agent_service` 是**轻 CPU、高并发连接**型（会话管理、上下文拼装）；`rag_service` 的**文档解析是重度 CPU 密集型**（PDF 解析、切片、分块）。合并会导致「只想聊天」的请求被迫与文档解析抢 CPU，出现不可预测的延迟抖动 |
| **资源规格差异大** | rag_service 需 `cpu: 1~4`、`memory: 2~4Gi`，且 HPA 应按「队列长度 + CPU」扩容；agent_service 的规格与扩容指标完全不同。合并后无法独立调优 |
| **依赖面差异大** | rag_service 依赖 **Milvus**（全新基础设施）；agent_service 不依赖。合并意味着 **Milvus 抖动会直接传导到会话主链路**——这是最关键的反对理由 |
| **可用性策略不同** | RAG 检索失败应**降级**（走纯 LLM 对话）；而会话管理失败是**真正的错误**。两者的错误处理策略在同一个服务里会互相污染 |
| **变更频率不同** | RAG 是快速演进的领域（切片策略、检索算法、重排模型会频繁迭代）；Agent 编排相对稳定。合并会导致 RAG 的实验性变更影响 Agent 服务稳定性 |
| **失败影响面** | 独立后，文档解析任务堆积、解析器 OOM **不会拖垮会话服务** |
| **团队协作** | 团队 10 人以上，可支持独立服务边界 |

### 反面考量：「RAG 就是 Agent 的一部分」这一观点的合理性

| 观点 | 回应 |
|---|---|
| 「RAG 检索结果就是给 Agent 用的，天然属于 Agent」 | 检索结果确实是 Agent 的输入，但**产出检索结果是独立能力**——正如「数据库给业务用」不代表业务服务该内嵌数据库 |
| 「多一次 RPC 调用，增加延迟」 | 一次内网 RPC 约 **0.5–1 ms**，相比 Embedding 调用（数十 ms）+ Milvus 检索（数十至百 ms）**完全可忽略**。与 ADR-002 的结论一致：跳数不是决策依据 |
| 「部署更复杂」 | 属实，但复杂度换来了资源隔离、独立扩缩容与故障隔离，性价比合理 |
| 「小团队维护不动」 | 团队 10 人以上，且已有 5 个服务的运维体系，增量成本边际递减 |

### 后果

**正面**

- **故障隔离**：Milvus 故障、解析任务堆积不影响 Agent 会话主链路
- **独立扩缩容**：解析高负载时可单独扩容至 12 副本，其它服务不受影响
- **资源规格独立**：可为 CPU 密集场景单独配置 requests/limits
- **变更隔离**：RAG 的快速迭代不危及 Agent 服务稳定性
- **职责清晰**：`rag_service` 管「知识与检索」，`agent_service` 管「编排与对话」

**负面（需主动管理）**

- 多一个服务：多一套 Deployment、监控、CI 流水线
- 调用链变长：`agent → rag → llm` 三段链路，问题定位需依赖全链路追踪
- 需处理 PG ↔ Milvus 的一致性（已在 §4.9.4 设计补偿机制）
- 服务间接口需稳定化（IDL 先行）

**缓解措施**

| 风险 | 措施 |
|---|---|
| 调用链变长 | OpenTelemetry 全链路 TraceID 贯穿；关键节点打点 |
| 一致性复杂 | PG 为内容真相源 + `vec_status` 标记 + 补偿任务；删除靠 PG 侧过滤兜底 |
| 服务数增加 | 复用统一的服务模板（Dockerfile / Deployment / 配置加载），边际成本低 |
| Milvus 不可用 | 三级降级：PG 关键词检索 → 空上下文 → 纯 LLM，响应标注 `degraded` |
| RAG 拖慢对话 | 设置严格超时（检索 P99 < 500ms），超时即降级 |

### 重新评估触发条件

1. 团队规模缩至 **3 人以下**，无法维护 6 个服务
2. RAG 能力被**完全剥离**为独立产品线
3. 实测表明 RAG 检索开销**长期低于 5ms**（此时合并不再有性能顾虑）
4. 服务间调用的稳定性问题**持续造成事故**，且无法通过降级化解

### 相关决策

- [ADR-001](#adr-001--服务按业务能力垂直拆分5-个服务) —— 本决策是该原则在 RAG 场景的延续
- [ADR-002](#adr-002--llm_service-保持独立不合并进-agent_service) —— 同类问题的先行论证
- [ADR-006](#adr-006--向量库选用-milvus-而非-pgvector) —— RAG 的存储选型
- [ADR-009](./08-ADR-009-JSON引擎抽象层.md) —— JSON 引擎抽象层

---

## 决策索引

| 编号 | 决策 | 状态 |
|---|---|---|
| ADR-001 | 服务按业务能力垂直拆分（5 个服务） | ✅ 已采纳 |
| ADR-002 | llm_service 保持独立，不合并进 agent_service | ✅ 已采纳 |
| ADR-003 | Eino 锁定稳定线 v0.9.21，Adapter 隔离 | ✅ 已采纳 |
| ADR-004 | 统一 Go 1.26 + Sonic ≥ v1.15.0 | ✅ 已采纳 |
| ADR-005 | 关系型数据库选用 PostgreSQL 18（替代 MySQL 8.0） | ✅ 已采纳 |
| ADR-006 | 向量库选用 Milvus 3.0.x（而非 pgvector） | ✅ 已采纳（2026-10-08 修订版本线） |
| ADR-007 | rag_service 独立部署，不并入 agent_service | ✅ 已采纳 |
| [ADR-008](./07-ADR-008-mock向量语义与阈值解耦.md) | mock 向量必须具备语义结构，且阈值与生产解耦 | ✅ 已采纳 |
| [ADR-009](./08-ADR-009-JSON引擎抽象层.md) | JSON 引擎走 `pkg/jsonx` 抽象层，Sonic 用 `ConfigStd` 对齐标准库行为 | ✅ 已采纳 |
| ADR-010 | 模型接入百炼 compatible-mode，BaseURL 由 `endpoint()` 归一化，密钥按索引注入 | ✅ 已采纳 |
| [ADR-011](./09-ADR-011-rerank融入检索链路.md) | 重排融入检索链路：外扩前打分、失败降级但不静默 | ✅ 已采纳 |

> **注**：ADR-001 中「5 个服务」为采纳时的数量。后续新增 `rag_service`（ADR-007）后，服务总数为 **6 个**。ADR-001 的编号与内容按维护约定保持原样，不修改历史记录。
>
> **另注**：ADR-008 ~ ADR-011 有独立成文的文件（编号与文件名前缀不对齐，见 `docs/` 目录）。原因是 007 之后的决策需要展开「背景—问题—决策—后果」的完整论证，塞进本文档会破坏既有 ADR 的单页结构。

---

## 附：待决策事项

| 事项 | 说明 | 相关文档 |
|---|---|---|
| Milvus cluster 切换时机 | standalone 单节点的数据量上限与切换 cluster 的触发指标 | [02-技术架构设计.md §4.11](./02-技术架构设计.md) |
| Milvus 3.0 新特性启用时机 | 何时启用 Storage V3（换取 Snapshot / TEXT 字段）与提升索引版本（换取 SINDI 等新算法）。**启用即关闭回滚窗口，需在此之前建立独立备份恢复点** | [05-部署运维手册.md §4.10.1](./05-部署运维手册.md) |
| Rerank 模型选型 | Cross-Encoder 本地部署 vs 调用厂商 Rerank API（成本与效果权衡） | [02-技术架构设计.md §4.9.3](./02-技术架构设计.md) |
| 混合检索是否默认开启 | 向量 + 关键词（RRF 融合）是默认策略还是可选项 | [02-技术架构设计.md §4.9.3](./02-技术架构设计.md) |
| Nacos 生产部署形态 | 自建 StatefulSet 或使用云托管 | [05-部署运维手册.md](./05-部署运维手册.md) |

---

## ADR-010 模型接入百炼 compatible-mode，BaseURL 由代码归一化

| 项 | 内容 |
|---|---|
| **状态** | ✅ 已采纳（2026-10-09） |
| **相关** | ADR-003（Provider 选型） |

### 背景

平台统一模型出口接入阿里云百炼（DashScope）兼容模式：
`https://maas.qianwenaiapi.com/compatible-mode/v1`，OpenAI 协议。

首批接入 6 个模型：4 个对话 + 1 个向量 + 1 个重排。

### 实测发现（决定了本 ADR 的内容）

| 发现 | 影响 |
|---|---|
| 百炼文档给的 BaseURL **已含 `/v1`**，而代码原实现是 `BaseURL + "/v1/chat/completions"` | 直连拼出 `/v1/v1/chat/completions` → **404**，且本地日志看不出 BaseURL 写重复了 |
| `/v1/models` 能列出 **262 个**模型，但 4 个对话模型实调全返回 `403 AccessDenied.Unpurchased` | **列出 ≠ 已开通**。若据列表配置，生产启动后才发现全部不可用（控制台开通后已复测通过） |
| `qwen3.7-text-embedding-flash` 实测输出 **1024 维** | 与既有 Milvus schema（`dim: 1024`）恰好一致，无需迁移 |
| rerank 的 `/reranks`、`/v1/reranks` 均 404 | 该平台疑似未开放 OpenAI 兼容 rerank端点 |
| `applyEnv` 只支持标量，**不支持切片元素** | 6 个模型无法各自注入 Key，只能把密钥写进配置文件——而配置文件进镜像层，`docker history` 可见 |

### 决策

1. **BaseURL 归一化下沉到 `endpoint()` 函数**（`pkg/provider/rawhttp`）。
   兼容带/不带 `/v1` 两种写法，避免把厂商的 URL 书写习惯变成调用方的负担。
   回归测试 `pkg/provider/rawhttp/endpoint_test.go` 锁定该行为。

2. **`applyEnv` 增加按索引覆盖**（`AEGIS_LLM__MODELS__<序号>__<字段>`），
   保持「显式逐字段声明」的既有风格，不引入反射式通用映射。
   理由：显式优于隐式——新增字段时必须有人记得补映射，不会静默失效。

3. **模型能力按实测标注，不按文档臆测**。
   未开通的对话模型保留在清单里（`status: enabled`），
   一旦控制台开通即生效；rerank 因端点未确认，先 `status: disabled`。

4. **新增 `make model-probe`**：只探测向量模型。
   chat 探测要花钱，而 embedding 几乎免费且能同时验证
   *BaseURL 拼接 → 鉴权 → 模型可用 → 维度自洽* 四件事。

### 后果

- **正面**：换厂商只需改 `base_url` 与模型名，不动代码；
  密钥不再有烤进镜像层的风险；配置是否生效可用 `make model-info` 直接确认。
- **代价**：`endpoint()` 的"聪明"带来一层隐式行为 ——
  若将来某个网关的 BaseURL 本身就以 `/v1` 结尾且需要 `/v1/v1`，
  归一化会误判。已由测试固定预期，改动时必须同步改测试。
- **遗留**：rerank 需确认该平台真实端点后再启用；
  `rawHTTPProvider` 的 ToolCall / Reasoning 能力待开启（模型已支持，客户端未解析）。

---

## 补记：模型能力必须实测，不可按命名推测（2026-10-09 复检）

控制台开通后复检，得到实测能力矩阵，与最初按模型名推测的值有 **3 处偏差**：

| 模型 | 推测 | 实测 | 判定方式 |
|---|---|---|---|
| `qwen3.8-27b` | reasoning ❌ | **✅** | 响应含 `reasoning_content`（61 字符/ 16 tokens） |
| `qwen3.7-flash` | reasoning ❌ | **✅** | 响应含 `reasoning_content`（600 字符 / 194 tokens） |
| `qwen3.7-flash` | tool_call ❌ | ❌（确认） | 带 `tools` 时 `finish_reason=stop`、`tool_calls=null` |

另确认流式可用：`stream:true` 返回 125 个 `data` 分片 + `[DONE]`，
且每帧 `delta` 同时含 `content` 与 `reasoning_content`。

### 为什么这条值得单独记

`qwen3.7-flash` 是「闪电」系列，直觉上就该是轻量、无工具调用；
`qwen3.8-27b` 参数较小，也容易被认为不做推理。
**两个直觉都部分错了**。

若按推测把 `qwen3.7-flash` 标成 `tool_call: true`，上层会把工具定义发过去 ——
白花钱，且模型返回 `finish_reason=stop`，工具调用永远拿不到，
排查时还会怀疑「是不是框架 bug」。

> ★ 结论固化：模型能力接入流程中，
> **`configs/*.yaml` 的能力字段必须由实测产出**，
> 并用 `make live-check` 纳入部署前检查。
> 这也是本轮新增该命令的直接原因。

### Provider 能力与模型能力的区别（易混淆点）

复检时另发现：`rawHTTPProvider.Capabilities()` 返回
`{ToolCall:false, Reasoning:false}`，但 4 个模型里有 3 个真支持 tool_call、
4 个都支持 reasoning。

**两者不是一回事**：
- `Configs` 里 `supports_tool_call` = **模型**的能力
- `Provider.Capabilities()` = **本客户端实现**的能力

薄客户端的 `chatResponse` / `streamFrame` 只解析 `content`，
未解析 `tool_calls` 与 `reasoning_content`，故如实报 false。

这是 ADR-003「不假装支持」的延续：**宁可报 false 让上层关掉特性，
也不要假装支持后拿到空结果**。开启路径已在代码注释中标注。

