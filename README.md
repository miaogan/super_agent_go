# Aegis · AI 智能体服务平台

> 基于 **CloudWeGo**（Kitex + Hertz + Eino）生态构建的企业级 AI 微服务平台，使用 **Nacos** 统一承担服务注册发现与配置中心。

---

## 项目简介

Aegis 是一套面向企业内部的 AI 智能体应用后端平台，由 6 个微服务组成，统一提供：

- 🔐 **认证与权限** —— 注册登录、JWT 鉴权、RBAC、API Key 管理
- 👤 **用户体系** —— 用户资料、组织架构、角色权限、配额管理
- 🤖 **Agent 能力** —— 会话管理、多轮上下文、工具调用（ReAct）、流式对话
- 🧠 **模型网关** —— 多厂商统一接入（OpenAI 兼容协议）、智能路由、故障降级、Token 计量、Embedding
- 📚 **RAG 知识库** —— 文档解析、切片、向量化、语义检索、引用溯源

---

## 服务架构

```
客户端 / 业务方
      │ HTTPS
      ▼
┌──────────────────────────────────────────────────┐
│  gateway（Hertz · HTTP :8080）                    │
│  路由 · 鉴权 · 限流 · 协议转换 · SSE              │
└──┬────────┬────────┬────────┬────────┬───────────┘
   │        │        │        │        │  Kitex RPC
   ▼        ▼        ▼        ▼        ▼
┌───────┐┌───────┐┌───────┐┌───────┐┌───────────┐
│ auth_ ││ user_ ││agent_ ││ llm_  ││  rag_     │
│:8881  ││:8882  ││:8883  ││:8884  ││ :8885     │
└───────┘└───────┘└───┬───┘└───┬───┘└─────┬─────┘
   │        │         │        │          │
   │        │         └────────┤          │ Embedding 调用
   │        │          RAG 召回 │          ▼
   │        │                  │    ┌──────────┐
   │        │                  └───▶│  Milvus  │
   │        │                       │ 向量检索  │
   └────────┴────────┬──────────────┴──────────┘
                     │
   ┌─────────────────┴─────────────────┬─────────────┐
   ▼                                   ▼             ▼
┌──────────┐  ┌──────────────────┐  ┌──────────┐
│  Nacos   │  │ PostgreSQL 18 +  │  │ 外部 LLM │
│注册+配置 │  │ Redis            │  │   API    │
└──────────┘  └──────────────────┘  └──────────┘
```

> `agent_service → rag_service → llm_service` 构成 **RAG 增强对话链路**。RAG 检索失败时降级为纯 LLM 对话（响应标注 `degraded`），**主链路不受影响**。

---

## 技术选型

| 层次 | 选型 | 说明 |
|---|---|---|
| **Go 版本** | **Go 1.26** | Sonic 原生加速支持的最后一个版本；Green Tea GC 降 10–40% GC 开销 |
| RPC 框架 | **Kitex** | 高性能 Go RPC，内置服务治理 |
| HTTP 框架 | **Hertz** | 高性能 Go HTTP，网关使用，默认集成 Sonic |
| JSON 序列化 | **Sonic ≥ v1.15.0** | JIT + SIMD 加速，序列化快 2–3 倍 |
| LLM 编排 | **Eino v0.9.21** | Go 生态最成熟的 LLM 框架；**锁稳定线，不用 alpha** |
| 网络库 | **Netpoll** | 非阻塞 I/O，Kitex/Hertz 默认底座 |
| IDL | **Protobuf (proto3)** | 跨语言友好，Kitex 完整支持 |
| 注册 / 配置中心 | **Nacos 2.x** | 一套组件解决两件事 |
| **关系型数据库** | **PostgreSQL 18** | JSONB + 原生分区 + 部分索引；PG 18 原生异步 I/O |
| **向量数据库** | **Milvus 3.0.2** | 千万级至十亿级 ANN 检索；Go SDK 走 `client/v3`，Eino 侧自研 `pkg/vectordb/milvus3` 适配 |
| 缓存 | **Redis 7.x** | 缓存 / 会话 / Embedding 与检索结果缓存 |
| Embedding | OpenAI 兼容 Embeddings | 经 llm_service 统一出口，默认 `bge-m3`（1024 维） |
| 文档解析 | Eino Document Loader | 内置 PDF/DOCX/HTML/XLSX/MD 解析器 |
| 模型接入 | OpenAI 兼容协议 | 通过 `base_url` 切换厂商 |
| 可观测 | OpenTelemetry + Prometheus | 全链路追踪 + 指标 |
| 部署 | Docker Compose（测试）/ K8s（生产） | 开发友好 + 弹性伸缩 |

> ### ⚠️ Go 版本强约束
>
> **统一使用 Go 1.26.x，禁止升级到 1.27+。**
>
> **原因**：Sonic 官方兼容矩阵为 `go1.17~1.26 (amd64)` / `go1.20~1.26 (arm64)`，**Go 1.26 是 Sonic 原生加速支持的最后一个版本**。升级到 1.27+ 后 Sonic 会静默回退到 `encoding/json`，JSON 性能退回基线。
>
> **同时必须保证 Sonic ≥ v1.15.0**：Go 1.26 将内置 map 重写为 Swiss Table，Sonic ≤ v1.14.1 会因 `undefined: GoMapIterator` **直接编译失败**（非性能降级）。

> ### ⚠️ Milvus 版本与安全强约束
>
> **本项目锁 `milvusdb/milvus:v3.0.2`**，Go 客户端锁 `github.com/milvus-io/milvus/client/v3 v3.0.2`，且 **9091 端口严禁对外暴露**。
>
> **必须显式加固（3.0 默认值不安全）**：
> - **3.0 默认 `authorizationEnabled: false`**，且默认 root 密码为 **`Milvus`** —— 必须显式开启授权并修改默认密码
> - 9091 端口绑定 `127.0.0.1`（CVE-2026-26190，CVSS 9.8：低版本该端口默认无认证，完整 REST API 暴露，可导致数据篡改、删库乃至 RCE）
> - 修改 `etcd.rootPath` 默认值（`by-dev` 是漏洞利用链的一环）
> - MinIO 9000/9001 不对外暴露，凭据非默认值
>
> **必须守住回滚能力**：`common.storage.useLoonFFI` **保持 `false`**；`targetVecIndexVersion` / `targetScalarIndexVersion` **不提升**。一旦启用这两项并落盘，回滚 2.6 不可逆。
>
> **与 eino-ext 的关系**：官方 `milvus2` 组件依赖 `client/v2`，**与 3.0 的 `client/v3` 类型不互通**，因此本项目自研 `pkg/vectordb/milvus3`（约 300 行）实现 Eino 的 `Indexer`/`Retriever` 接口。详见 [技术架构设计 §4.8](./docs/02-技术架构设计.md)。

---

## 文档目录

| 文档 | 内容 | 适合读者 |
|---|---|---|
| [00-CloudWeGo生态调研.md](./docs/00-CloudWeGo生态调研.md) | 选型依据、组件能力边界、**踩过的坑与放弃的候选方案** | 新成员、选型评审 |
| [01-产品需求文档-PRD.md](./docs/01-产品需求文档-PRD.md) | 产品定位、目标用户、功能需求（FR-1~FR-6）、业务流程、非功能需求、里程碑 | 产品、业务、研发负责人 |
| [02-技术架构设计.md](./docs/02-技术架构设计.md) | 总体架构、工程结构、Nacos 集成、核心组件设计、**Milvus 选型与 RAG 设计**、高可用与安全 | 架构师、后端研发 |
| [03-API接口文档.md](./docs/03-API接口文档.md) | 全部 HTTP 接口定义（含 **RAG 接口**）、统一错误码、5 份 Protobuf IDL | 前后端研发、对接方 |
| [04-数据库设计.md](./docs/04-数据库设计.md) | **PostgreSQL 18 表结构**、MySQL→PG 语法映射、Redis Key 规范、容量规划、**RAG 数据设计**、初始化脚本 | 后端研发、DBA |
| [05-部署运维手册.md](./docs/05-部署运维手册.md) | Docker Compose 部署、K8s 部署、监控告警、故障排查、**Milvus 运维**、应急预案 | 运维 / SRE |
| [06-架构决策记录-ADR.md](./docs/06-架构决策记录-ADR.md) | **9 条关键决策的背景、备选方案、依据与后果** | 全体研发、新成员入职 |
| [07-ADR-008-mock向量语义与阈值解耦.md](./docs/07-ADR-008-mock向量语义与阈值解耦.md) | mock 向量为何必须有语义结构、检索阈值为何与生产解耦 | 研发、调试 RAG 链路 |
| [08-ADR-009-JSON引擎抽象层.md](./docs/08-ADR-009-JSON引擎抽象层.md) | Sonic/标准库双实现、**为何必须用 `ConfigStd` 而非 `ConfigDefault`**、启动自检与 strict 模式 | 关注性能与跨平台兼容的研发 |

> 💡 **新成员建议阅读顺序**：README → ADR（理解为什么这么设计）→ PRD（理解做什么）→ 技术架构（理解怎么做）

---

## 快速开始

### 零依赖冒烟（推荐先跑这个）

不需要 Nacos、PostgreSQL、Milvus 或任何模型密钥，mock 模式可在纯进程内跑通全链路：

```bash
cd aegis
source scripts/goenv.sh     # 修正 GOROOT/PATH

# 一键启停（自动编译、等待健康检查、打印失败日志尾部）
make dev                   # 等价于 ./scripts/dev.sh up
./scripts/dev.sh status    # 查看各服务健康状态
./scripts/dev.sh down

# 端到端冒烟：77 个断言，覆盖 auth → user → llm → rag → agent → gateway
bash scripts/smoke.sh
```

预期输出末尾为全部通过。冒烟覆盖 10 个方面：健康检查、鉴权边界、
登录/刷新、Agent CRUD、会话、知识库 CRUD、文档上传（multipart + JSON）、
检索、同步对话、SSE 流式、越权防线。

> ★ **SSE 断言会校验响应头与事件序列**
>（`text/event-stream`、`X-Accel-Buffering: no`、
> `session → retrieval → delta×N → done`）——
> 这类问题在浏览器里表现为"推了几个token 就断流"，不看响应头很难定位。

### 单元测试

```bash
source scripts/goenv.sh
make test                # 全量（含 -race）
make cover               # 生成 coverage.html
make check-sonic         # ★ Sonic 路径的 CI 门禁
```

### Docker Compose 部署（完整依赖）

```bash
# 0. 由 .proto 重新生成 Kitex 代码（改了 proto 才需要）
make gen-proto

# 1. 中间件：PG / etcd / MinIO / Nacos / Milvus
docker compose up -d postgres etcd minio nacos milvus
docker compose ps              # 等健康检查通过（Milvus 首启 60–90s）

# 2. 业务服务
docker compose up -d auth_service user_service llm_service rag_service agent_service gateway

# 3. 冒烟验证
bash scripts/smoke.sh
```

Mock 变体（不连任何中间件，省掉数 GB 内存）：

```bash
docker compose --profile mock up -d       # 6 个 mock 服务
```

Sonic 变体（JSON 引擎换 JIT，`strict=true` 不符即拒启动）：

```bash
docker compose --profile sonic build
docker compose --profile sonic up -d
docker compose logs gateway-sonic | grep json_engine   # 确认引擎
```

> ⚠️ **关于 mock 阈值**：mock 用稀疏词袋模拟向量，相似度上限约 0.24（数学约束），
> 因此 mock 模式使用独立阈值 `AEGIS_RAG__MOCK_SCORE_THRESHOLD`（默认 0.1），
> **生产的 `default_score_threshold=0.3` 不受影响**。
> 详见 [ADR-008](./docs/07-ADR-008-mock向量语义与阈值解耦.md)。

---

## 工程结构

> ★ 与代码同步（2026-10）。目录变更请一并更新本节
> 与 [02-技术架构设计.md §3.2](./docs/02-技术架构设计.md)。

```
aegis/
├── go.work / go.work.sum       # workspace：统一 9 个模块
├── Makefile                    # ★ 统一入口（24 个目标）
├── Dockerfile                  # ★ 单一构建定义（参数化服务 + JSON 引擎）
├── docker-compose.yml          # 15 服务 + 3 profile（mock / sonic）
│
├── idl/                        # ★ 接口定义 + kitex_gen（独立模块）
│   ├── commonv1/ authv1/ userv1/ agentv1/ llmv1/ ragv1/
│   │└── *.proto + 手写消息体
│   └── kitex_gen/              # 自动生成，禁止手改
│
├── gateway/                    # Hertz 网关（SSE / 鉴权 / 错误码映射）
│   ├── cmd/
│   └── internal/{router,middleware,handler,sse,client}/
│
├── auth_service/               # 认证（OAuth2.0 客户端 + 自签 JWT）
│   └── internal/{handler,service,dal,token,idp}/
│
├── user_service/               # 用户
│   └── internal/{handler,service,dal}/
│
├── llm_service/                # 模型统一出口（Chat / ChatStream / Embed）
│   └── internal/{handler,service,router,provider,metering}/
│
├── rag_service/                # RAG（入库 / 检索 / 三级降级）
│   └── internal/{handler,knowledge,ingest,retriever,store,dal,job}/
│
├── agent_service/              # 智能体编排
│   └── internal/{handler,orchestrate,store,planner}/
│
├── pkg/                        # ★ 共享库（9 个服务共用，独立模块）
│   ├── serverx/                #   启动骨架：配置/日志/Nacos/probe/优雅退出
│   ├── jsonx/                  #   ★ JSON 引擎抽象（Sonic / 标准库双实现）
│   ├── conf/                   #   配置结构 + AEGIS_ 环境变量覆盖
│   ├── logger/ errno/ jwtx/ passwd/ cache/ identity/
│   ├── mockx/                  #   故障注入（冒烟开关）
│   ├── pagination/
│   ├── vectordb/               #   向量库抽象 + mockstore
│   └── provider/               #   LLM 抽象：rawhttp（零依赖）/ chain / mock
│
├── scripts/                    # ★ 工程化脚本
│   ├── goenv.sh                #   统一 Go 环境（GOROOT/GOTOOLCHAIN）
│   ├── dev.sh                  #   本地一键启停
│   ├── smoke.sh                #   ★ 端到端冒烟（77 断言）
│   ├── gen_proto.sh            #   proto → kitex_gen
│   └── sonic_version.sh        #   解析 sonic 版本供 ldflags 注入
│
├── tools/smokeclient/          # 冒烟客户端（独立模块）
├── configs/config.example.yaml # 配置示例
├── docs/                       # ★ 文档集（与代码同仓）
└── .run/                       # 运行时产物（不入版本库）
```

### 常用命令

```bash
make help            # 列出全部目标
make build           # 编译 6 个服务到 .run/bin
make build-sonic     # ★ 用 Sonic 引擎编译（高性能路径）
make test            # 全量测试（-race）
make env-audit       # ★ 审计环境变量名是否真能被 conf.go 读取
make milvus-probe    # ★ 真实探测 Milvus 适配层（需 Milvus 已启动）
make milvus-diag     # ★ 诊断 Milvus 一致性行为（写后多久可检索）
make check-sonic     # ★ Sonic 路径的 CI 门禁
make engine-info     # ★ 打印当前 JSON 引擎（两种构建对照）
make bench-json      # ★ JSON 引擎性能对比
make gen-proto       # 由 .proto 重新生成 kitex_gen
make verify          # 编译 → 检查 → 冒烟 全流程
```

---

## 模型接入（阿里云百炼compatible-mode）

```bash
make model-info    # ★ 打印实际生效的模型清单（密钥脱敏）
make model-check   # ★ 校验配置文件合法 + 维度自洽
make model-probe   # ★ 真实探测连通性（仅探向量模型，几乎不产生费用）
make live-check    # ★ 用项目 Provider 真实调用各对话模型（产生少量费用）
```

### 当前接入的 6 个模型

能力标注**全部来自实测**，不是按模型名推测：

| # | 模型 | 类型 | tool_call | reasoning | priority |
|---|------|------|:---------:|:---------:|----------|
| 0 | `qwen3.8-max` | 对话 | ✅ | ✅ | 10 |
| 1 | `deepseek-v4-pro-0813` | 对话（推理） | ✅ | ✅ | 20 |
| 2 | `qwen3.8-27b` | 对话 | ✅ | ✅ | 30 |
| 3 | `qwen3.7-flash` | 对话（轻量） | ❌ | ✅ | 40 |
| 4 | `qwen3.7-text-embedding-flash` | 向量（**1024 维**） | — | — | 10 |
| 5 | `qwen3.7-text-rerank` | 重排（**默认启用**） | — | — | 90 |

> ★ **实测修正过 3 处按推测填错的地方**（2026-10-09）：
> `qwen3.8-27b` 与 `qwen3.7-flash` 的 reasoning 实测**都支持**
> （分别返回 61 / 600 字符 reasoning_content）；
> 而 `qwen3.7-flash` 的 tool_call 实测**真不支持** —— 带 `tools` 时
> `finish_reason=stop`、`tool_calls=null`。
>
> **教训**：模型能力必须实测，不能按命名规律猜。
> 标错 `tool_call: true` 会让上层把工具定义白发给一个不支持的模型，
> 既花钱又拿不到工具调用。

### 降级链

```
qwen3.8-max → deepseek-v4-pro-0813 → qwen3.8-27b → qwen3.7-flash
```

注意 `deepseek` 与 `qwen3.8-27b` 同样支持 reasoning，与 `max` 的差别在深度与成本，
而非「有无推理能力」。

### 重排（rerank）已融入检索链路

`rag.rerank_enabled` 默认 **true**。检索链路为：

```
embedding → Milvus 召回 20 条 → 回 PG 取原文
          → Rerank 20 条 → 阈值过滤 → 取 top 5
          → 上下文外扩（只对这 5 条）→ 拼 Prompt
```

**重排失败不影响主链路**：未配置模型、缺 `rerank_path`、超时、Provider 报错，
都会退回按向量相似度排序，并在响应的 `degraded` / `degrade_reason` 里如实标注。

启用需要**同时**满足两个条件 —— 很容易只做第一个：

1. `llm.models` 里 rerank 模型 `status: enabled`
2. 该模型填了 `rerank_path`（如 `text-rerank/text-rerank`）

> ★ 第 2 条是最容易踩的坑：`rerank_path` 填的是 DashScope 原生路径里的**服务名**，
> 不是模型名。实测（同一 Key、同一请求体，只改 URL 路径段）：
>
> | 路径 | 结果 |
> |---|---|
> | `text-rerank/text-rerank` | ✅ 200 |
> | `qwen3.7-text-rerank/text-rerank` | ❌ 400 url error |
> | `text-rerank/qwen3.7-text-rerank` | ❌ 400 url error |
> | `qwen3.7-text-rerank/qwen3.7-text-rerank` | ❌ 400 url error |
>
> 两种写法字面只差一个 `qwen3.7-` 前缀，而报错只说「url error」，
> 完全不提「路径段用错」。留空时本服务直接跳过重排，而不是拼一个必然 400 的 URL。

验证：

```bash
make rerank-probe   # 真实调用，打印分数与排序是否合理
```

实测输出（5 条语义分层语料，耗时 178ms）：

```
第1名  分数=0.9984  ✓ 保留      原文[0]: RAG是检索增强生成…
第2名  分数=0.4239  ✓ 保留      原文[3]: 向量检索通过计算查询向量…
第3名  分数=0.2390  ✗ 低于阈值   原文[4]: Rerank（重排）是对召回的…
```

设计与取舍详见 [ADR-011](docs/09-ADR-011-rerank融入检索链路.md)。

### ⚠️ Provider 能力 ≠ 模型能力

`rawHTTPProvider` 上报 `Capabilities{ToolCall:false, Reasoning:false}`，
**即使模型真支持** —— 因为薄客户端的 `chatResponse`/`streamFrame`
只解析 `content`，没解析 `tool_calls` 与 `reasoning_content`。

> 「能力由实现决定，不由模型决定」是 ADR-003 的刻意设计：
> 宁可如实报 false 让上层关闭特性，也不要假装支持后拿到空结果。
> 要开这两个能力需在结构体里加字段并做映射，届时再改 `Capabilities`。

### 实测记录

```
$ make live-check
provider=rawhttp  capabilities={ToolCall:false Reasoning:false Embeddings:true Streaming:true}

[qwen3.8-max]              ✓ 1.58s  in=71 out=30 total=101
[deepseek-v4-pro-0813]     ✓ 2.38s  in=71 out=56 total=127
[qwen3.8-27b]              ✓ 2.65s  in=71 out=41 total=112
[qwen3.7-flash]            ✓ 1.45s  in=71 out=27 total=98
通过 4  失败 0
```

> ★ 注意 `deepseek-v4-pro-0813` 会先输出 `reasoning_content` 再给 `content`。
> `max_tokens` 设小了会导致 `content` 为空字符串 ——
> 本项目默认给到 512+，实测正常。
> `live-check` 会把「成功但内容为空」单独报出来。

### 密钥管理

密钥**不写进仓库**：`.env` 被 `.gitignore` 与 `.dockerignore` 双重排除，
容器化时用环境变量注入：

```bash
# .env（本地，已排除）
AEGIS_LLM__MODELS__0__API_KEY=sk-xxxx
AEGIS_LLM__MODELS__4__API_KEY=sk-xxxx
```

> ★ `llm.models` 是结构体切片，每个模型可配**独立的 Key 与 BaseURL**。
> 为此 `pkg/conf` 的 `applyEnv` 增加了按索引覆盖的能力
> （`AEGIS_LLM__MODELS__<序号>__<字段>`），否则只能把密钥烤进配置文件——
> 而配置文件会被打进镜像层，`docker history` 可见。

### ★ BaseURL 带不带 `/v1` 都可以

OpenAI 兼容网关的 BaseURL 写法业界不统一。`rawhttp` 的 `endpoint()` 会自动归一化：

```go
// BaseURL https://host/v1  + /v1/chat/completions → https://host/v1/chat/completions
// BaseURL https://host     + /v1/chat/completions → https://host/v1/chat/completions
```

> 实测踩过：百炼文档给的是 `.../compatible-mode/v1`，照抄进配置后
> 直连拼出 `.../compatible-mode/v1/v1/chat/completions` → **404**，
> 且本地日志看不出「BaseURL 多写了一段 `/v1`」。
> 回归测试见 `pkg/provider/rawhttp/endpoint_test.go`。

### 探测脚本做了什么

`make model-probe` 一次性验证四件事同时成立：

```
BaseURL 拼接正确 → 鉴权通过 → 模型名可用 → 输出维度与 Milvus schema 一致
```

```
[4] qwen3.7-text-embedding-flash     (向量)
    ✓ 向量维度 1024，与配置及 Milvus schema 一致
```

> ★ 脚本刻意**只探测向量模型**：chat 模型每调一次都产生费用，
> 而 embedding 几乎免费且能同时验证上述四件事，性价比最高。
> chat 模型是否开通，用控制台或一次性 `curl` 确认即可。

---

## JSON 引擎（Sonic）

项目选用 Go 1.26 的目的之一是集成 Sonic。这部分通过 `pkg/jsonx` 落地：

```bash
make build            # 默认：encoding/json，跨平台最稳
make build-sonic      # Sonic JIT（amd64/arm64 才有加速）
make engine-info      # 确认当前跑在哪个引擎上
```

启动日志恒定打印引擎信息：

```
json_engine=sonic json_version=v1.15.0 json_native=true
json_engine=encoding/json json_version=go1.26.0 json_native=false \
  json_fallback_reason="当前平台支持 sonic，但构建未启用 -tags sonic"
```

实测性能（linux/amd64）：

| 场景 | encoding/json | sonic | 提升 |
|------|---------------|-------|------|
| `Marshal` 结构体 | 1141 ns/op, 11 allocs | 745 ns/op, 4 allocs | **1.53x** |
| `Unmarshal` | 3516 ns/op | 1286 ns/op | **2.73x** |
| `Marshal` map（LLM 请求体） | 1101 ns/op, 15 allocs | 648 ns/op, 4 allocs | **1.70x** |

> ★ Sonic 侧刻意使用 `ConfigStd` 而非更快的 `ConfigDefault`：
> 后者不转义 HTML、不排序 map key，会导致 **JWT 签名在两个引擎下不一致**。
> 详见 [ADR-009](./docs/08-ADR-009-JSON引擎抽象层.md)。

生产环境建议开启严格自检，把「以为用了 Sonic、实际没有」这类静默降级挡在启动阶段：

```bash
AEGIS_JSON__ENGINE=sonic AEGIS_JSON__STRICT=true ./.run/bin/gateway
#不符即拒绝启动，而不是静默降级
```

---

## 核心设计要点

### 0. Go 1.26 + Sonic 版本协同（最重要的前置约束）

这是全项目**最容易踩坑、且踩了要重来**的地方，务必先读：

```
Go 1.26  ─────►  Sonic ≥ v1.15.0  ─────►  Hertz/Kitex 获得 JSON 原生加速
   │                    │
   │                    └─ 低版本直接编译失败（GoMapIterator undefined）
   │
   └─ 1.27+  ────►  Sonic 静默回退 encoding/json（性能退回基线）
```

**三条铁律**

1. **Go 版本锁 1.26.x**，`go.work` / `go.mod` / CI / Dockerfile 四处统一，禁止 `latest`
2. **Sonic 锁 ≥ v1.15.0**（当前 v1.15.0），版本号由 `scripts/sonic_version.sh`
   解析并经 `-ldflags -X` 注入二进制，启动日志可确认实际版本
3. **业务代码不得直接 import Sonic**，一律走 `pkg/jsonx`
   （详见下方「JSON 引擎」章节与 [ADR-009](./docs/08-ADR-009-JSON引擎抽象层.md)）

> ★ **踩过的坑**：最初的想法是「直接 import sonic，不支持的平台自然编译失败」。
> 但事实是 Sonic 自带 `compat.go`，在不支持的平台**依然能编译通过**，
> 只是内部回落 `encoding/json` —— 不报错、功能全对、性能悄悄退化。
> 这种「静默降级」比编译失败危险得多，因此改为显式抽象层 + 启动自检。

**收益**（Go 1.26 独有）

| 特性 | 价值 |
|---|---|
| Green Tea GC 默认开启 | GC 开销降 10–40%，利好高频小对象场景 |
| cgo 调用开销降 ~30% | Sonic 的 C 核心函数调用更快 |
| 切片栈上分配优化 | 减少堆分配 |
| `crypto/tls` 抗量子混合密钥交换 | 传输层安全增强 |

### 0.2 Eino 版本策略：锁稳定线，不用 alpha

| 线路 | 版本 | 用途 |
|---|---|---|
| **稳定线** | **v0.9.21**（2026-09-23） | ✅ 生产依赖 |
| alpha 线 | v0.10.0-alpha.35 | ⚠️ 仅实验，禁止进生产 |

> **别被「稳定版」三个字迷惑**：Eino 处于 0.x 阶段，v0.9 稳定线同样有过大规模 API 废弃（`Agent Transfer`、`Workflow Agent`、`Supervisor`、`AgentMiddleware` 全被废弃或标记不推荐）。**「稳定」= 更少 bug + 有 migration notes，不等于接口冻结。**
>
> v0.10 alpha 平均 **2–3 天一个版本**，官方明确提示仍可能 Breaking Changes。

**应对：Adapter 强制隔离**

```
app/agent_service（业务层）        ← 零 Eino import
        │
        ▼
pkg/llm/provider.Provider          ← 自研稳定接口（含 Capabilities 能力声明）
        │
        ├── einoProvider           ← Eino 只出现在这 1 个文件（完整能力，默认启用）
        └── rawHTTPProvider        ← 裸 HTTP 薄客户端（能力子集，降级启用）
```

这样 Eino 破坏性升级的影响面被压缩到一个文件，且随时可回退到零依赖方案。

**两条实现在能力上是不对等的**——兜底层刻意只覆盖最小子集，规模约 150 行：

| 能力 | `einoProvider` | `rawHTTPProvider` |
|---|---|---|
| Chat / ChatStream（纯文本）/ Embed | ✅ | ✅ |
| ToolCall / Reasoning / 多模态 | ✅ | ❌ |
| 第三方依赖 | Eino + eino-ext | **仅标准库 + Sonic** |

能力差异通过接口的 `Capabilities()` **显式声明**，上层在兜底模式下明确降级（如禁用工具调用），而非运行时崩溃。**兜底层不引入任何第三方 SDK**——否则"最后一道防线"会被同一条供应链同时击穿。

**四条规约**

1. 生产锁 `v0.9.21`，`replace` 防间接依赖拖入 alpha
2. 业务代码**禁止**直接 import Eino
3. `einoProvider` 与 `rawHTTPProvider` 共用一套契约测试
4. 升级必须：读 migration notes → 契约测试全绿 → 端到端回归 → 灰度

> **RAG 侧同样遵守此原则**：rag_service 使用 eino-ext 的 Loader / Splitter 组件，内层再封一层自研 `ingest.Loader` 接口；**向量读写则完全自研**（`pkg/vectordb`，生产实现 `milvus3` 待接入，当前 mock 走 `mockstore`）——因 eino-ext 的 `milvus2` 组件依赖 `client/v2`，与 Milvus 3.0 的 `client/v3` 不互通。详见 [§0.4](#04-milvus-部署要点) 与 [技术架构设计 §4.8](./docs/02-技术架构设计.md)。

### 0.3 PostgreSQL 18 与 Milvus 的分工

**数据分治**——两类数据特征完全不同，分而治之：

| 数据 | 存放 | 理由 |
|---|---|---|
| 知识库/文档元数据、**chunk 正文** | **PostgreSQL 18** | 强事务、关系约束、状态机、全文检索 |
| **chunk 向量** | **Milvus 3.0.2** | 高维 ANN 检索 |
| 检索必需标量（`kb_id`/`doc_id`/`chunk_id`） | Milvus（冗余一份） | 支撑「标量过滤 + 向量检索」一次完成 |

**关键设计**：**Chunk 正文不存 Milvus，只存 PG**。这样 PG 是唯一的**内容真相源**，带来两个回报：

1. **Milvus 完全丢失也能重建**——从 PG `chunks` 表重新向量化即可恢复
2. **删除操作可先返回成功**——PG 侧软删后，检索结果回 PG 取正文时被过滤，Milvus 残留向量不会出现在结果里，异步清理即可

两侧一致性靠 **`vec_status` 标记 + 补偿任务**保证（PG 与 Milvus 无分布式事务，采用最终一致）。

### 0.4 Milvus 部署要点

Milvus **standalone** 需要 3 个组件（**不需要 Pulsar/Kafka**，Woodpecker 消息队列内置）：

| 组件 | 作用 | 关键要求 |
|---|---|---|
| etcd 3.5.18 | 元数据 | **必须 SSD**，IOPS > 500，否则频繁选举 |
| MinIO | 对象存储（向量与索引文件） | S3 兼容；9000/9001 不对外暴露 |
| Milvus 3.0.2 | 引擎 | 端口 19530(gRPC) / 9091(metrics) |

**硬件建议**：8 核 / **32 GB 内存**（索引常驻）/ NVMe SSD 500 GB。

**部署顺序**：`etcd` + `minio` → `milvus` → `rag_service`。Milvus 首次启动需 60–90 秒。

**安全基线（强制）**：9091 绑定 `127.0.0.1` + K8s NetworkPolicy 限制 + **显式开启 `authorizationEnabled`（3.0 默认 false）** + **修改默认 root 密码 `Milvus`** + `etcd.rootPath` 改非默认值。

**回滚基线（强制）**：`common.storage.useLoonFFI` 保持 `false`；索引版本保持默认不提升。升级前必须做 etcd snapshot + MinIO 全量备份。

### 0.5 RAG 降级策略（可用性优先）

RAG 检索失败**不让整个请求失败**，采用三级降级：

```
Milvus 不可用
   ├─ ① PG 关键词检索（chunks 表 trgm 索引）→ 仍有结果，召回质量下降
   ├─ ② 上下文置空 → 走纯 LLM 对话
   └─ ③ 响应标注 degraded: true，前端提示「知识库暂时不可用」
```

**用户仍能得到回答**（只是可能不够准确）并收到明确提示。这是可用性优先于答案完备性的取舍。

### 0.6 容器构建：三个真实踩过的坑

这一节记录的都是**实测卡住、且报错具有误导性**的问题。

#### 坑一：GOPROXY 不显式指定 → 构建静默挂死

`golang:alpine` 的默认 `GOPROXY` 是 `https://proxy.golang.org,direct`。在国内网络
访问它会**长时间挂起直到超时**，构建日志停在

```
#9 [stage-0 2/16] RUN apk add --no-cache git
#10 [stage-0 3/16] RUN --mount=type=cache,target=/go/pkg/mod go mod download all
```

之后**一动不动，且没有任何错误输出**——看起来像 Docker 卡死，实际是网络挂起。
实测对照：

| 源 | 沙箱内实测 |
|---|---|
| `proxy.golang.org` | ❌ 25s 超时无响应（curl exit 124） |
| `goproxy.cn` | ✅ 200，0.29s |
| `mirrors.aliyun.com/goproxy` | ✅ 200，4.8s |

**修复**：`ARG GOPROXY=https://goproxy.cn,direct` + `GOSUMDB=sum.golang.google.cn`。
留成 ARG 而非写死，是为了内网有私有 Athens/Nexus 代理时用 `--build-arg GOPROXY=`覆盖。

#### 坑二：`.dockerignore` 不支持行内注释

```dockerignore
.run/  # 本地产物    ← 这行等于匹配「名为 .run/ 与 # 本地产物 的路径」
```

规则**完全失效且不报任何错**，表现为「配了 `.dockerignore` 但上下文没变小」。
本项目未排除时上下文 184MB（`.run/` 占 124MB），配好后 1.18MB。
**所有注释必须独占一行。**

#### 坑三：`.dockerignore` 匹配不区分「同名文件」与「同名目录」

为排除 smokeclient 的编译产物，写 `smokeclient` 或 `**/smokeclient` 会把
`tools/smokeclient/` **整个目录**排掉，于是依赖层直接失败：

```
failed to calculate checksum of ref ...: "/tools/smokeclient/go.mod": not found
```

而这个报错**完全不提 `.dockerignore`**，会让人误以为是 Docker 缓存问题、
反复 `--no-cache` 重试（本项目实际空转了3 轮）。正确写法是精确到文件路径：

```dockerignore
tools/smokeclient/smokeclient
/smokeclient
```

> ★ 裸规则（如 `aegis`）只匹配**同名文件**，不会误伤 `aegis_dir/` 这类目录——
> 但依赖层 `COPY dir/file` 会因目录被整体排除而失败，两者要分开理解。

**排查手法**：怀疑 `.dockerignore` 时，别猜，直接把上下文 dump 出来看真相。

```dockerfile
FROM alpine
COPY . /ctx
RUN find /ctx -type f | sort && find /ctx -type f | wc -l
```

本项目实测 141 个文件 / 1.18MB，规则生效一目了然。

#### 坑四：ARG / LABEL 不跨构建阶段传递

多阶段镜像里，构建阶段（第一个 `FROM`）的 `ARG` 和 `LABEL` **到运行阶段就失效了**。
实测症状：

```console
$ docker inspect aegis-gateway --format '{{json .Config.Labels}}'
null # ← LABEL 白写了
$ docker inspect aegis-gateway --format '{{range .Config.Env}}{{println .}}{{end}}' | grep SERVICE
SERVICE_NAME=                # ← 空值
```

`docker build` 只给一个 `UndefinedVar: Usage of undefined variable '$SERVICE'` 警告，
**不阻断构建**，很容易被忽略。后果是 Nacos 注册名退化、排查用的引擎标记形同虚设。

修复：运行阶段重新 `ARG`，且`LABEL` 只能写在最后一个 `FROM` 之后。

```dockerfile
FROM alpine:3.20
ARG SERVICE=gateway        # 必须重新声明
ARG SONIC_TAGS=""
LABEL aegis.json.engine="${SONIC_TAGS:-encoding/json}" \
      aegis.service="${SERVICE}"
ENV SERVICE_NAME=${SERVICE}
```

#### 坑五：APKINDEX 拉取慢到像卡死

`dl-cdn.alpinelinux.org` 国内可达但很慢（实测 `apk add` 单步50s+，整包174s）。
换清华源后 0.28s。同样留成`ARG APK_REPO=`，传空串即回退官方源。

### 0.7 镜像构建实测结果

`linux/amd64`、无缓存全量构建：

| 项 | std | sonic |
|---|---|---|
| 构建耗时 | 42s | 27s（依赖层缓存命中） |
| 镜像体积 | 37.3MB | 37.3MB |
| 构建上下文 | 1.18MB（141 文件） | 同 |
| 容器内 `json_engine` | `encoding/json` | `sonic` |
| 容器内 `json_version` | `go1.26.8` | `v1.15.0` |
| 容器内 `json_native` | `false` | `true` |
| `/healthz` | ✅ healthy | ✅ healthy |

用 `docker inspect` 看 label 区分镜像，比进容器 exec 看日志快得多：

```console
$ docker inspect aegis-gateway:sonic --format '{{json .Config.Labels}}'
{"aegis.json.engine":"sonic","aegis.service":"gateway"}
```

### 1. Nacos 双角色

| 角色 | 实现 | 关键点 |
|---|---|---|
| **注册中心** | `kitex-contrib/registry-nacos` | Server 侧 `WithRegistry`，Client 侧 `WithResolver` |
| **配置中心** | `nacos-sdk-go` config client | `ListenConfig` 实现热更新，无需重启 |

命名空间按环境隔离：`aegis-dev` / `aegis-test` / `aegis-staging` / `aegis-prod`。

### 2. 模型统一接入

用 **Eino 的 `openai.ChatModel`** 作为统一适配层，通过 `BaseURL` 参数切换厂商：

```go
chatModel, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
    BaseURL: m.BaseURL,   // 切换厂商只改这里
    APIKey:  m.APIKey,
    Model:   m.Name,
})
```

接入新厂商（通义 / Kimi / vLLM）**只需在 Nacos 配置新增一条记录，无需改代码**。

### 3. 流式对话链路

```
客户端 ──SSE──▶ gateway ──Kitex Stream──▶ agent_service ──▶ llm_service ──▶ LLM API
                    │                                              │
                    └────────── 逐 chunk Flush 透传 ◀──────────────┘
```

关键：网关必须使用 `SetBodyStreamWriter` 并在每个 chunk 后 `Flush()`；Ingress 必须关闭 buffering。

### 4. RAG 增强对话链路

```
客户端 ──▶ gateway ──▶ agent_service ──▶ rag_service ──▶ Milvus
                            ▲                 │
                            │                 └─▶ llm_service.Embed(query)
                            │
                            └── 拼装 Prompt（【参考资料】[1]..[5] + 问题）
                                        │
                                        ▼
                                  llm_service ──▶ LLM API
                                        │
                                        ▼
                            答案含 [n] 引用 → 解析写入 cited_chunk_ids
```

**文档入库流水线（写入链路，异步）**

```
上传 → 存对象存储/file_hash 去重 → 写 documents(status=0)
     → Loader 解析 → Splitter 切片 → 写 PG chunks(vec_status=0)
     → 批量 llm_service.Embed()（Redis 缓存命中则跳过）
     → 写 Milvus → 回填 milvus_pk, vec_status=1 → documents.status=2
```

### 5. 故障降级

```
主模型失败 → Redis 记录失败计数 → 达阈值熔断 → 按 priority 切换到备用模型
→ 仍失败 → 返回 50301

RAG 检索失败 → PG 关键词检索 → 空上下文纯 LLM → 标注 degraded: true
```

---

## 演进路线

| 版本 | 重点 |
|---|---|
| **V1.0** | 打通基础链路（当前文档范围） |
| V1.1 | 工具调用、Prompt 模板、上下文摘要 |
| **V1.2** | **RAG 上线：rag_service + Milvus + 基础向量检索** |
| V1.3 | RAG 效果优化：混合检索、Rerank 重排、查询改写 |
| V2.0 | Eino ADK / DeepAgent、多 Agent 协作、human-in-the-loop |
| V2.1 | 多模态（图片 / 语音 / 视频）、多模态 Embedding |
| V3.0 | Agent 市场、开放平台、多租户计费 |

---

## 参考资源

- [CloudWeGo 官网](https://www.cloudwego.io/zh/)
- [Kitex 文档](https://www.cloudwego.io/zh/docs/kitex/)
- [Hertz 文档](https://www.cloudwego.io/zh/docs/hertz/)
- [Eino 文档](https://www.cloudwego.io/zh/docs/eino/)
- [Kitex Nacos 集成](https://www.cloudwego.io/zh/docs/kitex/tutorials/third-party/service_discovery/nacos/)
- [Nacos 官方文档](https://nacos.io/zh-cn/docs/what-is-nacos.html)
- [PostgreSQL 18 文档](https://www.postgresql.org/docs/18/)
- [Milvus 文档](https://milvus.io/docs)
- [pgvector（备选方案）](https://github.com/pgvector/pgvector)
