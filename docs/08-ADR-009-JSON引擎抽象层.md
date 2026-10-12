# ADR-009 · JSON 引擎抽象层（Sonic / encoding/json 双实现）

| 属性 | 值 |
|---|---|
| **状态** | ✅ 已采纳 |
| **日期** | 2026-10-09 |
| **相关** | [ADR-004（统一 Go 1.26 + Sonic）](./06-架构决策记录-ADR.md#adr-004--统一-go-126--sonic-v1150)、`pkg/jsonx/` |
| **决策者** | 平台组 |

---

## 1. 背景

[ADR-004](./06-架构决策记录-ADR.md#adr-004--统一-go-126--sonic-v1150) 确定了统一 Go 1.26 + Sonic ≥ v1.15.0。
落地时遇到三个现实约束：

### 1.1 Sonic 并非全平台可用

Sonic 依赖 JIT 与平台相关指令集。它自己的 `compat.go` 带了构建标签：

```go
// +build !amd64,!arm64 go1.27 !go1.17 arm64,!go1.20
```

展开后命中任一条即回落`encoding/json`：

| 条件 | 含义 |
|------|------|
| `!amd64,!arm64` | 非 amd64/arm64 架构 |
| `go1.27` | Go 1.27+（Sonic 尚未适配） |
| `!go1.17` | Go 1.17 以下 |
| `arm64,!go1.20` | arm64 且 Go 1.20 以下 |

### 1.2 最危险的不是「编不过」，而是「静默降级」

若业务代码直接 `import "github.com/bytedance/sonic"`：

- 在不支持的平台**依然能编译通过**（Sonic 内部有 compat）
- 运行**不会报错**，功能全部正常
- 只有性能悄悄退化为标准库水平

这类问题在压测时才暴露成" inexplicitable 的延迟毛刺"，且几乎没人会联想到引擎没生效。

### 1.3 两个引擎的默认行为并不一致

这是最容易踩的坑 —— **不是性能问题，是正确性问题**：

| 行为 | `encoding/json` | `sonic.ConfigDefault` | `sonic.ConfigStd` |
|------|-----------------|------------------------|--------------------|
| `EscapeHTML` | `true`（转义 `<>&`） | `false` | `true` |
| `SortMapKeys` | 总是排序 | 不排序 | `true` |
| `NoNullSliceOrMap` | `false`（nil → `null`） | `false` | `false` |

Sonic 自带两套预置配置，注释已写明：

```go
ConfigDefault = Config{}.Froze()          // "aiming at efficiency and safety"
ConfigStd     = Config{EscapeHTML: true,
                      SortMapKeys: true, ...}.Froze()  // "compatible with encoding/json"
```

**对本项目的致命影响**：`pkg/jwtx` 签发 JWT 时，
header 与 claims 序列化后的字节是 HMAC/RSA 签名的输入。
若签发侧用 `ConfigDefault`（`SortMapKeys=false`），字节序随 map 遍历随机变化，
而 Go 的 map 遍历顺序本身就是随机的 —— 于是
「本服务签的令牌被自己拒绝」。

更隐蔽的是 `EscapeHTML`：`user_profile` 这类含 `<script>` 的文本，
两个引擎产出的字节串不同，base64 后自然不同。

---

## 2. 决策

### 2.1 引入 `pkg/jsonx` 作为唯一 JSON 入口

```
业务代码 ──→ jsonx.Marshal / Unmarshal / NewEncoder / NewDecoder
                    │
        ┌───────────┴───────────┐
   engine_std.go          engine_sonic.go
   //go:build !sonic      //go:build sonic
   encoding/json          sonic.ConfigStd
```

**签名固定，引擎可换**。业务代码不感知底层实现。

### 2.2 Sonic 侧必须用 `ConfigStd` 而非 `ConfigDefault`

```go
// pkg/jsonx/engine_sonic.go
var api = sonic.ConfigStd
```

用 Sonic 的性能，换 `encoding/json` 的行为。
代价是失去 `EscapeHTML=false` 带来的那部分性能收益，
换来的是**两个引擎字节级一致**。

> 该选择由 `TestMarshalBytesIdenticalToStd` 锁死：
> 一旦有人改成 `ConfigDefault`，测试立刻失败。

### 2.3 引擎选择是构建期的，不是运行期的

```bash
go build ./...              # encoding/json（默认，跨平台最稳）
go build -tags sonic ./...  # Sonic JIT（amd64/arm64 才有加速）
```

**明确不做运行期开关** —— 未编译进去的代码不可能在运行期凭空启用，
那样的开关是假的。配置项 `json.engine` 的语义因此是
「校验期望」而非「切换引擎」。

### 2.4 启动自检 + strict 模式

```go
// pkg/serverx/serverx.go
if err := checkJSONEngine(ctx, cfg); err != nil {
    return err   // strict 模式下拒绝启动
}
```

| `json.engine` | `json.strict` | 行为 |
|---------------|---------------|------|
| `auto`（默认） | — | 不校验，用编译进来的引擎 |
| `sonic` | `false`（默认） | 不符时打 WARN 并继续（本地开发友好） |
| `sonic` | `true` | **不符时拒绝启动**（生产推荐） |
| `std` | `false` | 同上|
| `std` | `true` | 不符时拒绝启动 |
| 非法值 | — | 拒绝启动（配置错误不该静默忽略） |

启动横幅恒定打印引擎信息：

```
json_engine=sonic json_version=v1.15.0 json_native=true
json_engine=encoding/json json_version=go1.26.0 json_native=false \
  json_fallback_reason="当前平台支持 sonic，但构建未启用 -tags sonic"
```

### 2.5 平台原生性用构建标签判定，不用运行时解析

`pkg/jsonx/platform_native.go`：

```go
//go:build (amd64 || arm64) && !go1.27 && go1.17 && (!arm64 || go1.20)
```

与 Sonic 的标签严格互补。**不用 `runtime.Version()` 字符串解析**的理由：

- 构建标签由工具链判定，与 Sonic 自身用的是同一套语义，不会双标
- `devel` 版本（如 `go1.27-devel-xxxx`）用字符串解析极易误判

此外 `Info()` 还会用 `sonic.APIKind` 二次校验「实际走的是哪条实现」，
即使条件写宽了也只是多报告一句可用，不会把标准库误报成 Sonic。

---

## 3. 实测数据

### 3.1 性能（`make bench-json`，linux/amd64）

| Benchmark | encoding/json | sonic | 提升 |
|-----------|---------------|-------|------|
| `Marshal`（结构体） | 1141 ns/op, 11 allocs | 745 ns/op, **4 allocs** | **1.53x** |
| `Unmarshal` | 3516 ns/op, 27 allocs | 1286 ns/op, 23 allocs | **2.73x** |
| `Marshal`（map，OpenAI 请求体） | 1101 ns/op, 15 allocs | 648 ns/op, **4 allocs** | **1.70x** |

### 3.2 收益最大的场景

`llm_service` 的流式响应**每 token 一次 JSON 序列化**，
`gateway` 每个请求至少一次请求解析 + 一次响应序列化。
在这两条路径上，2x 以上的提升是直接的业务收益。

---

## 4. 后果

### 4.1 正面

- **跨平台安全**：非 amd64/arm64 平台零成本回落，不需要维护分支
- **行为一致**：两个引擎字节级一致，JWT 签名、缓存键、幂等比较都安全
- **可观测**：`make engine-info` 一条命令确认引擎，启动日志恒定打印
- **静默降级被堵住**：`strict=true` 让配置错误在启动阶段暴露

### 4.2 代价

- 业务代码不能直接用 `encoding/json` 的完整 API
  （`pkg/jsonx` 只暴露两引擎交集，如 `Encoder` 不含 `SetEscapeHTML`）
- Sonic 侧失去 `ConfigDefault` 的部分性能收益（换取行为一致，值得）
- CI 需维护两条构建路径（`make check` + `make check-sonic`）

### 4.3 需要注意的语义陷阱

即便用了 `ConfigStd`，仍有一处**标准库与Sonic 一致但反直觉**的行为：

> `json.Unmarshal` 到 `any`（`map[string]any`）时，所有数字都是 `float64`。
> `int64` 上限会变成 `9223372036854776000`（末位丢失）。

Sonic 另有 `UseInt64` 选项可改变此行为 —— 开启后同一段 JSON 在两个引擎下
解出不同 Go 类型。`ConfigStd` 未开启它，故本项目无此问题。

**业务约束**：需要精确大整数时，必须解到带类型的字段，不要落到 `map[string]any`。

---

## 5. 实施要点

| 文件 | 职责 |
|------|------|
| `pkg/jsonx/jsonx.go` | 公共 API（`Marshal`/`Unmarshal`/…）+ `EngineInfo` |
| `pkg/jsonx/engine_std.go` | 标准库实现（`//go:build !sonic`） |
| `pkg/jsonx/engine_sonic.go` | Sonic 实现（`//go:build sonic`），用 `ConfigStd` |
| `pkg/jsonx/platform.go` | `Info()` 入口 + `NativeSupported()` |
| `pkg/jsonx/platform_native.go` | 原生平台判定（构建标签） |
| `pkg/jsonx/platform_fallback.go` | 回落平台判定（构建标签） |
| `pkg/jsonx/version_sonic.go` | 接收 `-ldflags -X` 注入的版本号 |
| `pkg/conf.JSONConf` | `engine` / `strict` / `print_engine` |
| `pkg/serverx.checkJSONEngine` | 启动自检 |
| `scripts/sonic_version.sh` | 解析 sonic 版本供 ldflags 注入 |
| `Makefile` | `build-sonic` / `test-sonic` / `check-sonic` / `engine-info` / `bench-json` |
| `Dockerfile` | `SONIC_TAGS` / `SONIC_VER` 构建参数 |
| `docker-compose.yml` | `sonic` profile（4 个服务，`strict=true`） |

### `pkg/go.mod` 的特殊注意事项

```go
require (
	// ★ sonic 是构建标签门控的依赖
	github.com/bytedance/sonic v1.15.0
)
```

**必须显式 require 而非留作 indirect**：
`go mod tidy` 不识别自定义构建标签，会判定 sonic 未被使用
而将其降级或删除，导致下一个人 tidy 后 sonic 构建突然失败。

### Dockerfile 的参数陷阱

```dockerfile
# ✗ 错误：SONIC_VER 为空时整个 -ldflags 消失，镜像不做符号剥离
${SONIC_VER:+-ldflags "-X ...=$SONIC_VER -s -w"}

# ✓ 正确：两个变量分开处理
-ldflags "${SONIC_VER:+-X github.com/aegis/pkg/jsonx.sonicVer=$SONIC_VER }${LDFLAGS}"
```

---

## 6. 相关文档

- [02-技术架构设计.md §3.3 关键约定](./02-技术架构设计.md)
- [05-部署运维手册.md §3.4 Dockerfile 构建参数](./05-部署运维手册.md)
- [ADR-004 统一 Go 1.26 + Sonic](./06-架构决策记录-ADR.md#adr-004--统一-go-126--sonic-v1150)
