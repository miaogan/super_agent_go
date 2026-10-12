# ReAct 工具执行循环实现计划

## Context

当前 agent_service 的 `Orchestrator` 把 `plan.Tools []string`（工具名列表）传给 LLM，但 LLM 返回 `tool_calls` 后只收集不执行——`consumeStream()` 收集到 `toolCalls` 后函数结束，这些 call 既没落库也没被循环执行。要让 Agent 具备"调用 HTTP 工具 → 拿结果 → 再问 LLM → 最终回答"的完整能力，需要打通整条链路。

**已知可复用的基础设施：**
- `configs/postgres/init.sql` 已有 `tools` 表（type/http, endpoint, method, params_schema JSONB, timeout_ms）和 `agent_tools` 表（agent ↔ tool 绑定）
- `pkg/fetch` 封装好带 SSRF 防护的 HTTP 客户端
- `pkg/ssrf` 前置校验 + `fetch.DialContext` 二次校验
- `idl/agentv1/agent.proto` ChatMessage.role 已支持 `"tool"`、Agent.tools 已是 `repeated string`
- `idl/llmv1/llm.proto` Message 已有 `role="tool"` + `name` + `tool_calls` 字段
- `pkg/provider` 定义了 `Tool{Name, Description, Parameters(JSON Schema)}` 和 `ChatRequest.Tools []Tool`

**关键障碍（需要修复）：**
1. **rawhttp Capabilities().ToolCall = false** + Chat/Stream 里显式拒绝 Tools——注释明确说"由实现决定"，但模型实际支持（qwen3.8-max 等）
2. **rawhttp chatResponse/streamFrame 没解析 tool_calls 字段**
3. **llm_service.toProviderRequest 完全没处理 req.GetTools()**（tools 字段丢失）
4. **agent_service Orchestrator 只有一轮 LLM 调用**——没有 for 循环：LLM → tool_call? → 执行 → tool result → 再喂回 LLM → 直到 finish_reason=stop
5. **agent_service.BuildMessages 明确跳过 role!=user/assistant**（注释"工具消息当前不参与"）

## 实施步骤

### Step 1: rawhttp — 开启 ToolCall 支持（低风险基础设施，测试先行）

**文件：** `pkg/provider/rawhttp/rawhttp.go`

a) 修改 `chatResponse` 结构体：Message 里加 `ToolCalls []struct{ID, Name, Arguments string}` 和 `FinishReason string`

b) 修改 `streamFrame` 结构体：Delta 里加 `ToolCalls []struct{Index int, ID, Name string, Arguments string}`（SSE 增量格式）

c) 修改 `Chat()` 方法：
   - 删除 `ToolCall=false` 拒绝逻辑（`len(req.Tools) > 0 && !p.Capabilities().ToolCall` 那段）
   - 把 `req.Tools` 序列化进 body `"tools": [...]`（按 OpenAI 兼容格式 `{type:"function", function:{name, description, parameters}}`）
   - 响应里解析 `tool_calls` 到 `provider.ChatResponse.ToolCalls`
   - 根据 finish_reason=tool_calls 设置 `resp.FinishReason`

d) 修改 `ChatStream()` 方法：
   - 同样加 tools 进请求 body
   - 增量收集 tool_calls（SSE tool_calls 是分片追加的，需要累积 JSON arguments）
   - `Chunk` 里加 `ToolCalls []provider.ToolCall` 字段

e) 修改 `Capabilities()` 把 `ToolCall: true`（但 Reasoning 保持 false——reasoning_content 解析也是独立工作，这轮不做）

f) 写测试 `tool_call_test.go`：httptest server 返回 tool_call 响应，验证解析正确；流式验证增量拼接 arguments

### Step 2: llm_service — 透传 tools + 解析 tool_calls

**文件：** `llm_service/internal/service/llm.go`

a) `toProviderRequest()` 里新增 tools 转换：`req.GetTools()` → 需要解析成 `[]provider.Tool`，但当前 pb 的 tools 字段是 `repeated string`（工具名列表），不是完整定义
   → 方案：在 llm_service 启动时查 PG tools 表缓存一份 `map[string]conf.ToolDef`，`toProviderRequest` 时把名字列表转完整定义。或者**更轻量**：在 Agent 创建时把工具定义（JSON Schema 描述）已经存在 PG 里，agent_service 构造 `ChatReq` 时直接传完整 Tool 定义（proto3 扩展成 Tool 对象列表）

   简化方案——**proto 不动**，在 llm_service service 层加 `ToolRegistry` 接口：
   ```go
   type ToolResolver interface {
       ResolveTools(ctx context.Context, names []string) ([]provider.Tool, error)
   }
   ```
   默认实现从 PG tools 表读；mock 测试直接返回空

b) Chat() 响应里把 `provider.ChatResponse.ToolCalls` 正确填进 `llmv1.ChatResp.ToolCalls`（已经做了但上游没传 tools，一直没触发）

c) ChatStream() 同理——从 `Chunk` 里收集 tool_calls

### Step 3: agent_service Orchestrator — 实现 ReAct 循环（核心改动）

**文件：** `agent_service/internal/orchestrate/orchestrate.go`

核心模式：把原来单次调用改成 `for tool_round := 0; tool_round < max_tool_rounds; tool_round++` 循环

```
历史消息 + user query → LLM(messages, tools)
├── finish_reason = stop → 最终回答
├── finish_reason = tool_calls → 执行每个 tool_call
│   ├── 组装 tool result messages (role="tool", name=toolName, content=resultJSON)
│   └── 把 tool results append 到 messages
└── 循环回去，messages 里已有 assistant(tool_calls) + tool(result)
```

a) 新增 **ToolExecutor** 接口（Orchestrator 的 Options 里新增）：
```go
type ToolExecutor interface {
    Execute(ctx context.Context, toolName string, argumentsJSON string) (resultJSON string, err error)
}
```
HTTP 实现从 PG tools 表读 endpoint/method/timeout，用 `pkg/fetch` 执行，结果 JSON 序列化返回。

b) 重写 `Chat()`：
   - 先组装初始 messages（和现在一样）
   - for 循环：调 LLM → 有 tool_calls？→ 执行 → 把 assistant 消息 + tool result 追加进 messages
   - 每轮循环落库 assistant（含 tool_calls 标记）和 tool 消息
   - `max_tool_rounds` 硬上限（默认 5），超限返回截断
   - 最终 stop 的回答作为 Result.Content

c) 重写 `ChatStream()`：
   - 同样的 for 循环，但：
     - 第一轮流式 emit EventDelta/EventReasoning/EventToolCall（新事件类型）
     - 收到 tool_call → emit EventToolCall（前端可以显示"正在调用工具 xxx"）
     - 执行工具 → emit EventToolResult
     - 下一轮流式（可以继续 emit delta，但前端会看到第二轮）
   - 最终回答前 emit EventDone 带 citations/usage

d) 扩展 StreamEvent：加 `EventToolCall = "tool_call"` 和 `EventToolResult = "tool_result"` 类型

e) 修改 `BuildMessages`：
   - 不再跳过 role!=user/assistant —— 需要把 tool result 消息也喂回去
   - 但需要适配 provider.Message 的格式：role="tool" 时需要 content 和可选 ToolCallID

f) Orchestrator.Options 里新增 `ToolExec ToolExecutor`，`MaxToolRounds int`

### Step 4: AgentService 构造时注入依赖

**文件：** `agent_service/cmd/main.go`（或 `agent_service/internal/service/agent.go`）

a) 新增 `pgtoolstore` 实现 ToolExecutor —— 查 PG tools 表得到 endpoint/method/params_schema/timeout，HTTP 执行
b) 构造 Orchestrator.Options 时注入 ToolExec + MaxToolRounds=5

### Step 5: 端到端验证

a) 在 PG tools 表插入一个 demo HTTP 工具（如 `{name:"weather", endpoint:"https://httpbin.org/get", method:"GET", params_schema:{...}}`）
b) agent_tools 绑定 agent_id=1001 + tool_name="weather"
c) 重建 agent_service 镜像，重启
d) 登录 → agent 1001 chat → 问"帮我查一下..." → 应该看到 LLM 返回 tool_call → 执行 weather → tool result → LLM 再生成最终回答
e) 前端（如果有 UI 显示）可以看到工具调用过程

### Step 6: 测试

- rawhttp tool_call 解析（同步 + 流式增量）
- llm_service tool 透传 + tool_call 响应
- agent_service ReAct 循环 mock 测试（注入 fake ToolExecutor）：
  - 1 轮无 tool → 正常返回
  - 1 轮有 tool → 执行 → 第二轮 LLM → 正常返回
  - 超过 max_tool_rounds → 返回截断
  - tool 执行失败 → 错误消息喂回 LLM（让 LLM 换方式回答或道歉）

## 文件清单（新增/修改）

| 文件 | 改动类型 |
|------|----------|
| `pkg/provider/rawhttp/rawhttp.go` | 修改：开启 ToolCall Capabilities + 解析 tool_calls 字段 + 请求体加 tools |
| `pkg/provider/rawhttp/tool_call_test.go` | **新建**：同步 + 流式 tool_call 解析测试 |
| `pkg/provider/provider.go` | 修改：Chunk 结构体加 ToolCalls 字段 |
| `llm_service/internal/service/llm.go` | 修改：toProviderRequest 透传 tools；ChatStream 透传 tool_calls |
| `llm_service/internal/toolresolver/toolresolver.go` | **新建**：从 PG 查工具定义的 ToolResolver 接口+实现 |
| `agent_service/internal/orchestrate/orchestrate.go` | 重写：ReAct 循环 + BuildMessages 支持 tool 角色 + StreamEvent 扩展 |
| `agent_service/internal/toolexec/executor.go` | **新建**：ToolExecutor 接口 + HTTP 实现（用 pkg/fetch + ssrf） |
| `agent_service/internal/orchestrate/react_test.go` | **新建**：ReAct 循环单元测试（fake ToolExecutor） |
| `agent_service/internal/service/agent.go` | 修改：注入 ToolExec，PlanFrom 加载 Agent.tools 列表 |
| `agent_service/cmd/main.go` | 修改：Wire ToolExecutor 到 Orchestrator.Options |

## 约束

- **不改 proto**：保持 `repeated string tools`（工具名列表），llm_service 在运行时查 PG 补全定义
- **流式实现简化**：第一轮流式，如果第一轮就是 tool_call，第二轮再流式——不强求跨多轮保持"一个完整 SSE"；实际体验 tool_call 这种短序列 + 用户等待执行结果，前端会接受
- **params_schema 校验**：这轮不做 JSON Schema 验证，LLM 出什么就直接调 HTTP（生产应校验避免注入）
- **工具执行超时**：PG tools.timeout_ms 生效 + 一个全局上限（如 30s）
