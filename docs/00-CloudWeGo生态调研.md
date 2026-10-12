# CloudWeGo 生态调研（选型依据）

> **说明**：本文是 Aegis 平台的技术选型依据，记录「为什么选 CloudWeGo / Go 1.26 / Sonic」
> 以及各组件的能力边界与踩过的坑。
>
> 检索日期：2026-10。框架版本以 `go.work` 与各模块 `go.mod` 为准。
> 本文件已并入 `aegis/docs/`，与代码同仓，避免与实现脱节。

---

## 1. 选型结论

| 维度 | 选型 | 理由 |
|------|------|------|
| RPC 框架 | **Kitex v0.16.3** | 多协议、代码生成完善、可选 sidecarless Service Mesh |
| HTTP 框架 | **Hertz v0.10.6** | 与 Kitex 同源，网络层共享 Netpoll 优化 |
| Go 版本 | **1.26.x** | Green Tea GC、cgo 开销降约 30%，且是 Sonic 支持区间 |
| JSON 加速 | **Sonic ≥ v1.15.0** | 经 `pkg/jsonx` 抽象层接入，见 [ADR-009](./08-ADR-009-JSON引擎抽象层.md) |
| IDL | **proto3** | 多语言对接、字段兼容性规则明确 |
| 注册/配置中心 | **Nacos** | 兼具注册中心与配置中心，支持热更新 |
| 关系库 | **PostgreSQL 18** | 见 [ADR-005](./06-架构决策记录-ADR.md) |
| 向量库 | **Milvus 3.0.x** | 见 [ADR-006](./06-架构决策记录-ADR.md) |

---

## 2. CloudWeGo 组件能力

### 2.1 Kitex（RPC）

**核心能力**

- 多协议：Thrift / Protobuf / gRPC / HTTP，协议层用泛型编解码减少反射
- 代码生成：`kitex` 工具从 IDL 生成骨架，含 client、server、scaffold
- 服务治理：限流、熔断、重试、超时、负载均衡均以Option 形式挂载
- 可观测：支持 OpenTelemetry，可与 Nacos / ETCD / K8s 集成
- Service Mesh：可作 sidecarless 数据面对接 Istio，减少一跳网络延迟

**本项目用到的Option**

| Option | 用途 |
|--------|------|
| `server.WithRegistry` | Nacos 注册 |
| `server.WithMiddleware` | 拦截器（日志 / trace / mock 故障注入） |
| `client.WithHostPorts` | 本地开发直连 |
| `server.WithExitWaitTime` | 优雅退出的在途请求等待窗口 |
| `streaming.Stream` | 服务端流式（SSE 的 RPC 侧承载） |

**踩过的坑**

1. **跨进程错误串格式**：业务错误码在**前**，`[biz error]` 传输层标记在**后**

   ```
   remote or network error: rpc error: code = 13 desc = [40401] 资源不存在 [biz error]
   ```

   网关的错误码映射若用 `strings.LastIndex(msg, "[")` 会取到 `[biz error]`，
   导致 404 被误报成 500。必须从左往右找第一个纯数字方括号内容。

2. **`-service` 参数会在当前目录吐 `package main` 脚手架**：
   在库模块（如 `idl/`）里执行会生成 `main.go` / `handler.go`，
   破坏模块边界（报 `undefined: XxxServiceImpl`）。生成脚本需清理。

3. **`kitex` 生成参数组合**（缺一即报错）：

   ```bash
   kitex -type protobuf \
         -I . \
         -module github.com/aegis/idl \
         -gen-path kitex_gen \
         -service <svc> \
         ./<pkg>/<pkg>.proto
   ```

   - `-type protobuf` 必填，否则报 `can not guess an IDL type`
   - `-module` 必须是模块自身名，否则报 `not consist with go.mod`
   - `-gen-path` 必须相对路径，绝对路径触发 `panic: index out of range`
   - 最后一个参数必须是**文件路径**而非目录

### 2.2 Hertz（HTTP）

**核心能力**

- 基于自研 Netpoll 的高并发网络层
- API 设计接近 Gin（`GET`/`POST`/中间件/分组），迁移成本低
- 与 Kitex 共享底层优化，工程侧可统一版本节奏

**本项目要点**

1. **SSE 必须 `WriteTimeout = 0`**
   Hertz 默认 `WriteTimeout` 为 3s，长回答会在传输中途被掐断，
   客户端看到的是「推了几个token 就断了」。
   这是本项目 gateway 配置里最容易踩的一个坑。

2. **反向代理需手动关闭缓冲**
   SSE 响应要显式设置：

   ```
   Content-Type: text/event-stream
   Cache-Control: no-cache
   Connection: keep-alive
   X-Accel-Buffering: no   ← Nginx 默认会缓冲，破坏流式
   ```

### 2.3 Sonic（JSON 加速）

**为什么是 Sonic**：JIT +汇编实现，实测在本项目场景下
`Unmarshal` 快 2.73x、`Marshal` 快 1.5–1.7x，分配数从 11 降到 4。

**为什么必须做抽象层**：Sonic 自带 `compat.go`，
在不支持的平台/Go 版本上**能编译通过但静默回落到标准库**。
这不是编译错误，而是性能静默退化 —— 比崩溃难查得多。

**行为差异（关键坑）**

| 行为 | `encoding/json` | `ConfigDefault` | `ConfigStd` |
|------|-----------------|-----------------|-------------|
| EscapeHTML | `true` | `false` | `true` |
| SortMapKeys | 总排序 | 不排序 | `true` |

对 **JWT 签名**是致命的：header/claims 的序列化字节是签名输入，
字节不一致就意味着「本服务签的令牌被自己拒绝」。

**本项目决策**：用 `ConfigStd`，放弃 `ConfigDefault` 的部分性能，
换两个引擎字节级一致。详见 [ADR-009](./08-ADR-009-JSON引擎抽象层.md)。

---

## 3. 生态组件

### 3.1 Nacos

- 双重角色：服务注册 + 配置中心
- Kitex 侧通过 `kitex-contrib/registry-nacos/v2` 接入
- 配置热更新：通过监听 data_id 变更回调实现

**本项目的容错策略**：Nacos 不可用**不阻断启动**，
降级为本地直连并打WARN —— 否则一个配置中心故障会让整个平台停摆。

### 3.2 Milvus 3.0

**关键约束**：eino-ext 的 `milvus2` 组件依赖 `client/v2`，
与 Milvus 3.0 的 `client/v3` **不互通**。

因此本项目的向量读写完全自研（`pkg/vectordb`），
只在文档解析（Loader/Splitter）环节借用 eino-ext 组件。

详见 [ADR-006 向量库选用 Milvus](./06-架构决策记录-ADR.md#adr-006--向量库选用-milvus而非-pgvector)。

### 3.3 Eino

**版本策略**：锁 v0.9.x 稳定线，不用 alpha。

> ⚠️ **别被「稳定版」迷惑**：Eino 处于 0.x 阶段，v0.9 稳定线同样有过
> 大规模 API 废弃（`AgentTransfer`、`WorkflowAgent`、`Supervisor`、
> `AgentMiddleware` 全被废弃或标记不推荐）。
> 「稳定」= 更少 bug + 有 migration notes，不等于接口冻结。

**隔离方式**：业务层零Eino import，Eino 只出现在 `pkg/provider` 内部。

详见 [ADR-003 Eino 版本策略](./06-架构决策记录-ADR.md#adr-003--eino-锁定稳定线-v0921adapter-隔离)。

---

## 4. 候选方案对比与放弃理由

| 候选 | 放弃理由 |
|------|----------|
| Gin + gRPC-Go | 两者非同源，网络层与编解码无法共享优化；且 Gin 在纯RPC 场景无优势 |
| Kratos | 能力对等，但项目内未见Kitex 在字节内部大规模 RPC 场景的验证数据 |
| 单体 + 内网 RPC | 6 个服务的边界已明确（`agent` / `llm` / `rag` / `auth` / `user` / `gateway`），独立部署才能各自扩缩容 |
| llm 能力并入 agent_service | 会让 agent 承担模型路由/降级/计量，与编排职责冲突；且平台其他服务也需要模型出口 |
| Milvus 2.x | eino-ext 的 milvus2 适配器无法支持 3.0；强行用 2.x 放弃新特性 |
| pgvector | 数据量上到千万级后检索延迟不可接受；且需自行维护索引调优 |
| runtime 切换 Sonic | 未编译进去的代码无法在运行期凭空启用，这样的开关是假的 |

---

## 5. 参考资源

| 资源 | 用途 |
|------|------|
| [CloudWeGo 官网](https://www.cloudwego.io/) | 组件总览与文档入口 |
| [Kitex 仓库](https://github.com/cloudwego/kitex) | API 与 Option 参考 |
| [Hertz 仓库](https://github.com/cloudwego/hertz) | 路由与中间件参考 |
| [Sonic 仓库](https://github.com/bytedance/sonic) | 性能与兼容性说明 |
| [kitex-contrib](https://github.com/kitex-contrib) | Nacos 等中间件适配 |
| [Milvus 文档](https://milvus.io/) | client/v3 API |

> 官方基准数据（CloudWeGo 3.0 相对 2.x）：QPS +40%、P99 -25%、内存 -18%，
> 来源为字节服务框架团队公开材料。本项目的实测数据以仓库内
> `make bench-json` 与 `make engine-info` 输出为准。
