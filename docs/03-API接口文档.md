# AI 智能体服务平台 · API 接口文档

| 项目 | 内容 |
|---|---|
| **文档版本** | v2.0 |
| **Base URL** | `https://api.aegis.example.com`（生产） / `http://localhost:8080`（本地） |
| **API 版本** | v1 |
| **协议** | HTTPS + JSON（流式接口为 SSE） |

---

## 0. 通用约定

### 0.1 统一响应结构

**成功**

```json
{
  "code": 0,
  "message": "ok",
  "data": { },
  "trace_id": "abc123def456"
}
```

**失败**

```json
{
  "code": 40101,
  "message": "token expired",
  "data": null,
  "trace_id": "abc123def456"
}
```

### 0.2 统一错误码

| 错误码 | HTTP | 说明 | 处理建议 |
|---|---|---|---|
| `0` | 200 | 成功 | — |
| `40001` | 400 | 参数错误 | 检查请求参数 |
| `40002` | 400 | 参数校验失败 | 查看 message 明细 |
| `40101` | 401 | Token 缺失 | 携带 Authorization |
| `40102` | 401 | Token 无效 | 重新登录 |
| `40103` | 401 | Token 已过期 | 刷新 Token |
| `40104` | 401 | Token 已登出 | 重新登录 |
| `40301` | 403 | 无权限 | 申请权限 |
| `40401` | 404 | 资源不存在 | 检查 ID |
| `42901` | 429 | 触发限流 | 退避重试 |
| `42902` | 429 | 配额不足 | 提额或等待重置 |
| `50001` | 500 | 服务内部错误 | 联系平台 |
| `50301` | 503 | 模型服务不可用 | 重试或换模型 |
| `50302` | 503 | 下游服务不可用 | 重试 |
| `50401` | 504 | 请求超时 | 重试或缩短输入 |
| **`40410`** | **404** | **知识库不存在** | **检查 kb_id** |
| **`40411`** | **404** | **文档不存在** | **检查 doc_id** |
| **`40901`** | **409** | **文档重复（同库已有相同内容）** | **如需覆盖请先删除原文档** |
| **`42201`** | **422** | **文件格式不支持** | **支持 PDF/DOCX/HTML/XLSX/MD/TXT** |
| **`42202`** | **422** | **文件解析失败（加密/无文字层）** | **去除密码或改用文字版** |
| **`42203`** | **422** | **向量维度不匹配** | **联系平台核查知识库配置** |
| **`50701`** | **507** | **向量库不可用（Milvus 降级中）** | **响应含 `degraded: true`，可继续使用降级结果** |

### 0.3 认证方式

除登录、注册、健康检查外，所有接口需在 Header 携带：

```
Authorization: Bearer <access_token>
```

业务侧调用（服务间）可使用 API Key：

```
X-API-Key: sk-aegis-xxxxxxxxxxxxxxxx
```

### 0.4 通用请求头

| Header | 必填 | 说明 |
|---|---|---|
| `Authorization` | 是 | Bearer Token |
| `X-Request-Id` | 否 | 客户端请求 ID，不传则由网关生成 |
| `X-App-Id` | 否 | 应用标识，用于配额与统计 |
| `Content-Type` | 是 | `application/json` |

### 0.5 分页约定

请求：`page`（从 1 开始）、`page_size`（默认 20，最大 100）
响应：

```json
{
  "list": [],
  "total": 128,
  "page": 1,
  "page_size": 20
}
```

---

## 1. auth_service · 认证接口

### 1.1 用户注册

`POST /api/v1/auth/register`

**请求**

```json
{
  "username": "zhangsan",
  "password": "P@ssw0rd123",
  "email": "zhangsan@example.com",
  "phone": "13800138000"
}
```

| 字段 | 类型 | 必填 | 校验 |
|---|---|---|---|
| username | string | 是 | 4–32 位，字母数字下划线 |
| password | string | 是 | 8–32 位，需含大小写与数字 |
| email | string | 否 | 合法邮箱格式 |
| phone | string | 否 | 中国大陆手机号 |

**响应**

```json
{
  "code": 0,
  "message": "ok",
  "data": { "uid": "10086" },
  "trace_id": "..."
}
```

---

### 1.2 用户登录

`POST /api/v1/auth/login`

**请求**

```json
{
  "account": "zhangsan",
  "password": "P@ssw0rd123",
  "device": "web"
}
```

**响应**

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "access_token": "eyJhbGciOiJSUzI1NiIs...",
    "refresh_token": "rt_9f8e7d6c5b4a",
    "expires_in": 7200,
    "token_type": "Bearer",
    "user": {
      "uid": "10086",
      "username": "zhangsan",
      "nickname": "张三",
      "roles": ["user"]
    }
  },
  "trace_id": "..."
}
```

---

### 1.3 刷新 Token

`POST /api/v1/auth/refresh`

**请求**

```json
{ "refresh_token": "rt_9f8e7d6c5b4a" }
```

**响应**：同登录，返回新的 Token 对（Refresh Token 一次性轮换）。

---

### 1.4 登出

`POST /api/v1/auth/logout`

**请求**

```json
{ "all_devices": false }
```

**响应**

```json
{ "code": 0, "message": "ok", "data": { "success": true } }
```

---

### 1.5 校验 Token（内部接口）

`POST /api/v1/auth/verify` — 供网关调用

**请求**

```json
{ "access_token": "eyJhbGciOiJSUzI1NiIs..." }
```

**响应**

```json
{
  "code": 0,
  "data": {
    "valid": true,
    "uid": "10086",
    "roles": ["user"],
    "app_id": "app_001",
    "expires_at": 1759945200
  }
}
```

---

### 1.6 签发 API Key

`POST /api/v1/auth/apikey`

**请求**

```json
{ "name": "订单服务", "expire_days": 365 }
```

**响应**

```json
{
  "code": 0,
  "data": {
    "key_id": "key_001",
    "api_key": "sk-aegis-8f7d6c5b4a3e2d1c",
    "expires_at": 1791481200
  }
}
```
> ⚠️ `api_key` 仅创建时返回一次，请妥善保存。

---

## 2. user_service · 用户接口

### 2.1 获取当前用户信息

`GET /api/v1/user/profile`

**响应**

```json
{
  "code": 0,
  "data": {
    "uid": "10086",
    "username": "zhangsan",
    "nickname": "张三",
    "avatar": "https://cdn.example.com/a.png",
    "email": "z****n@example.com",
    "phone": "138****8000",
    "roles": ["user"],
    "status": 1,
    "created_at": "2026-01-01T00:00:00Z",
    "last_login_at": "2026-10-08T20:00:00Z"
  }
}
```

### 2.2 更新用户信息

`PUT /api/v1/user/profile`

```json
{ "nickname": "张三丰", "avatar": "https://cdn.example.com/b.png" }
```

### 2.3 用户列表（管理端）

`GET /api/v1/user/list?page=1&page_size=20&keyword=zhang&status=1`

**响应**

```json
{
  "code": 0,
  "data": {
    "list": [ { "uid": "10086", "username": "zhangsan", "roles": ["user"], "status": 1 } ],
    "total": 128, "page": 1, "page_size": 20
  }
}
```

### 2.4 角色列表

`GET /api/v1/user/roles`

```json
{ "code": 0, "data": { "list": [ { "role_id": "r_01", "name": "admin", "desc": "管理员" } ] } }
```

### 2.5 分配用户角色

`POST /api/v1/user/roles/assign`

```json
{ "uid": "10086", "role_ids": ["r_01", "r_02"] }
```

### 2.6 查询配额

`GET /api/v1/user/quota?app_id=app_001`

```json
{
  "code": 0,
  "data": {
    "app_id": "app_001",
    "quota_total": 10000000,
    "quota_used": 2345678,
    "quota_left": 7654322,
    "reset_at": "2026-11-01T00:00:00Z"
  }
}
```

---

## 3. agent_service · 智能体接口

### 3.1 创建会话

`POST /api/v1/agent/session/create`

```json
{
  "agent_id": "agent_001",
  "title": "产品咨询",
  "metadata": { "scene": "support" }
}
```

**响应**

```json
{
  "code": 0,
  "data": {
    "session_id": "sess_abc123",
    "agent_id": "agent_001",
    "title": "产品咨询",
    "created_at": "2026-10-08T21:00:00Z"
  }
}
```

### 3.2 会话列表

`GET /api/v1/agent/session/list?page=1&page_size=20`

```json
{
  "code": 0,
  "data": {
    "list": [
      {
        "session_id": "sess_abc123",
        "agent_id": "agent_001",
        "title": "产品咨询",
        "message_count": 12,
        "last_message_at": "2026-10-08T21:05:00Z"
      }
    ],
    "total": 8, "page": 1, "page_size": 20
  }
}
```

### 3.3 拉取消息历史

`GET /api/v1/agent/session/messages?session_id=sess_abc123&page=1&page_size=50`

```json
{
  "code": 0,
  "data": {
    "list": [
      { "msg_id": "m_001", "role": "user", "content": "你好", "created_at": "2026-10-08T21:00:10Z" },
      { "msg_id": "m_002", "role": "assistant", "content": "你好！有什么可以帮你的？", "tokens": 24, "created_at": "2026-10-08T21:00:12Z" }
    ],
    "total": 12, "page": 1, "page_size": 50
  }
}
```

### 3.4 删除会话

`DELETE /api/v1/agent/session/delete`

```json
{ "session_id": "sess_abc123" }
```

### 3.5 同步对话

`POST /api/v1/agent/chat`

**请求**

```json
{
  "session_id": "sess_abc123",
  "agent_id": "agent_001",
  "message": "帮我总结一下这份合同的风险点",
  "model": "gpt-4o",
  "temperature": 0.7,
  "max_tokens": 4096,
  "stream": false,
  "tools": ["contract_checker"]
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| session_id | string | 否 | 不传则自动创建新会话 |
| agent_id | string | 否 | 不传则使用默认 Agent |
| message | string | 是 | 用户输入，最长按模型上下文限制 |
| model | string | 否 | 指定模型，不传则路由默认 |
| temperature | float | 否 | 0–2，默认 0.7 |
| max_tokens | int | 否 | 默认 4096 |
| stream | bool | 否 | 是否流式，默认 false |
| tools | string[] | 否 | 允许 Agent 使用的工具白名单 |

**响应**

```json
{
  "code": 0,
  "data": {
    "session_id": "sess_abc123",
    "msg_id": "m_003",
    "content": "这份合同存在 3 个主要风险点……",
    "model": "gpt-4o",
    "finish_reason": "stop",
    "usage": { "prompt_tokens": 512, "completion_tokens": 768, "total_tokens": 1280 },
    "latency_ms": 2140,
    "tool_calls": [
      { "tool": "contract_checker", "status": "success", "duration_ms": 320 }
    ]
  }
}
```

### 3.6 流式对话（SSE）

`POST /api/v1/agent/chat/stream`

**请求**：同 3.5，`stream` 强制为 true。

**响应**：`Content-Type: text/event-stream`

```
data: {"type":"start","session_id":"sess_abc123","msg_id":"m_003","model":"gpt-4o"}

data: {"type":"delta","content":"这份"}

data: {"type":"delta","content":"合同"}

data: {"type":"tool_call","tool":"contract_checker","status":"running"}

data: {"type":"delta","content":"存在 3 个主要风险点"}

data: {"type":"usage","prompt_tokens":512,"completion_tokens":768,"total_tokens":1280}

data: {"type":"end","finish_reason":"stop","latency_ms":2140}

data: [DONE]
```

**事件类型**

| type | 说明 |
|---|---|
| `start` | 会话开始，返回 session_id / msg_id |
| `delta` | 增量文本片段 |
| `tool_call` | 工具调用状态变更 |
| `usage` | Token 用量（结束前推送） |
| `end` | 正常结束 |
| `error` | 异常，含 code / message |

**前端示例**

```javascript
const resp = await fetch('/api/v1/agent/chat/stream', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${token}` },
  body: JSON.stringify({ session_id, message: '你好' })
});

const reader = resp.body.getReader();
const decoder = new TextDecoder();
while (true) {
  const { done, value } = await reader.read();
  if (done) break;
  const text = decoder.decode(value);
  // 按 \n\n 切分 SSE 事件，解析 data: 后的 JSON
}
```

### 3.7 创建 Agent

`POST /api/v1/agent/create`

```json
{
  "name": "合同助手",
  "description": "分析合同风险",
  "system_prompt": "你是一位资深法务专家……",
  "model": "gpt-4o",
  "temperature": 0.3,
  "max_tokens": 4096,
  "tools": ["contract_checker"],
  "context_rounds": 20
}
```

**响应**

```json
{ "code": 0, "data": { "agent_id": "agent_001", "created_at": "2026-10-08T21:00:00Z" } }
```

### 3.8 Agent 列表 / 详情 / 更新 / 删除

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/agent/list` | Agent 列表（分页） |
| GET | `/api/v1/agent/detail?agent_id=agent_001` | Agent 详情 |
| PUT | `/api/v1/agent/update` | 更新 Agent 配置 |
| DELETE | `/api/v1/agent/delete` | 删除 Agent |

---

## 4. llm_service · 模型接口

### 4.1 模型列表

`GET /api/v1/llm/models`

```json
{
  "code": 0,
  "data": {
    "list": [
      {
        "name": "gpt-4o",
        "provider": "openai",
        "context_window": 128000,
        "max_output": 16384,
        "supports_tools": true,
        "supports_stream": true,
        "price_in": 0.0025,
        "price_out": 0.01,
        "status": "available"
      },
      {
        "name": "deepseek-chat",
        "provider": "openai",
        "context_window": 64000,
        "max_output": 8192,
        "supports_tools": true,
        "supports_stream": true,
        "price_in": 0.00014,
        "price_out": 0.00028,
        "status": "available"
      }
    ]
  }
}
```

### 4.2 统一对话接口

`POST /api/v1/llm/chat`

**请求**

```json
{
  "model": "gpt-4o",
  "messages": [
    { "role": "system", "content": "你是助手" },
    { "role": "user", "content": "你好" }
  ],
  "temperature": 0.7,
  "max_tokens": 4096,
  "tools": [
    {
      "name": "get_weather",
      "description": "查询天气",
      "params_schema": { "type": "object", "properties": { "city": { "type": "string" } } }
    }
  ],
  "tool_choice": "auto"
}
```

**响应**

```json
{
  "code": 0,
  "data": {
    "model": "gpt-4o",
    "message": { "role": "assistant", "content": "你好！有什么可以帮你的？" },
    "tool_calls": [],
    "finish_reason": "stop",
    "usage": { "prompt_tokens": 28, "completion_tokens": 16, "total_tokens": 44 },
    "fallback_from": null
  }
}
```

> `fallback_from` 非空时表示发生了降级，值为原模型名。

### 4.3 统一流式对话

`POST /api/v1/llm/chat/stream`

**响应**：SSE，事件同 3.6。

### 4.4 Token 用量统计

`GET /api/v1/llm/usage?start=2026-10-01&end=2026-10-08&app_id=app_001&granularity=day`

```json
{
  "code": 0,
  "data": {
    "summary": {
      "prompt_tokens": 12345678,
      "completion_tokens": 8765432,
      "total_tokens": 21111110,
      "cost": 123.45
    },
    "buckets": [
      { "date": "2026-10-01", "prompt_tokens": 1000000, "completion_tokens": 800000, "cost": 10.2 },
      { "date": "2026-10-02", "prompt_tokens": 1200000, "completion_tokens": 900000, "cost": 11.8 }
    ]
  }
}
```

### 4.5 Prompt 模板渲染

`POST /api/v1/llm/prompt/render`

```json
{ "template_id": "tpl_001", "variables": { "name": "张三", "topic": "合同" } }
```

**响应**

```json
{ "code": 0, "data": { "content": "你好张三，我们来聊聊合同……" } }
```

### 4.6 Embedding 向量化（内部接口，rag_service 调用）

`POST /api/v1/llm/embeddings`

由 `rag_service` 调用，不直接对外暴露业务用途。所有 Embedding 调用**必须经 llm_service 出口**，以统一配额校验、计量与缓存。

**请求**

```json
{
  "model": "bge-m3",
  "input": ["第一段文本", "第二段文本"],
  "encoding_format": "float"
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| model | string | 是 | Embedding 模型名 |
| input | string[] | 是 | 待向量化文本，单次最多 64 条（内部批处理用 16） |
| encoding_format | string | 否 | `float`（默认）\| `base64` |

**响应**

```json
{
  "code": 0,
  "data": {
    "model": "bge-m3",
    "dim": 1024,
    "embeddings": [
      { "index": 0, "vector": [0.0123, -0.0456, "..."] },
      { "index": 1, "vector": [0.0789, 0.0012, "..."] }
    ],
    "usage": { "prompt_tokens": 48, "total_tokens": 48 },
    "cached": [false, true]
  },
  "trace_id": "abc123def456"
}
```

> **`cached` 字段**：标记该条文本是否命中 Redis Embedding 缓存（`aegis:rag:embed:{sha256}`）。命中时跳过实际模型调用，`usage` 中不计入。这是控制 Embedding 成本的主要手段。

---

## 5. rag_service · 知识库接口（新增）

### 5.1 创建知识库

`POST /api/v1/rag/kbs`

**请求**

```json
{
  "name": "产品手册知识库",
  "description": "收录全部产品说明书",
  "visibility": 1,
  "embedding_model": "bge-m3",
  "chunk_size": 512,
  "chunk_overlap": 64,
  "splitter": "recursive",
  "top_k": 5,
  "score_threshold": 0.30
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| name | string | 是 | 知识库名称，1–128 字符 |
| description | string | 否 | 描述 |
| visibility | int | 否 | 1 私有（默认）/ 2 团队 / 3 公开 |
| embedding_model | string | 否 | 默认 `bge-m3`。**创建后不可更改** |
| chunk_size | int | 否 | 切片长度，默认 512，范围 128–2048 |
| chunk_overlap | int | 否 | 重叠长度，默认 64，需 < chunk_size |
| splitter | string | 否 | `recursive`（默认）/ `markdown` / `semantic` |
| top_k | int | 否 | 默认召回数，5 |
| score_threshold | float | 否 | 相似度阈值，默认 0.30 |

**响应**

```json
{
  "code": 0,
  "data": {
    "kb_id": 1001,
    "name": "产品手册知识库",
    "embedding_model": "bge-m3",
    "embedding_dim": 1024,
    "doc_count": 0,
    "chunk_count": 0,
    "status": 1,
    "created_at": "2026-10-08T21:00:00+08:00"
  }
}
```

> ⚠️ `embedding_dim` 由 `embedding_model` 决定，服务端自动推导（`bge-m3` → 1024），客户端无需传。**该值在 Milvus Collection 创建时固化，后续不可变更。**

### 5.2 知识库列表

`GET /api/v1/rag/kbs?page=1&page_size=20&keyword=产品`

**响应**

```json
{
  "code": 0,
  "data": {
    "list": [
      {
        "kb_id": 1001,
        "name": "产品手册知识库",
        "description": "收录全部产品说明书",
        "visibility": 1,
        "embedding_model": "bge-m3",
        "doc_count": 12,
        "chunk_count": 3480,
        "status": 1,
        "updated_at": "2026-10-08T21:00:00+08:00"
      }
    ],
    "total": 3,
    "page": 1,
    "page_size": 20
  }
}
```

### 5.3 知识库详情 / 更新 / 删除

```http
GET    /api/v1/rag/kbs/{kb_id}      # 详情
PUT    /api/v1/rag/kbs/{kb_id}      # 更新（name/description/visibility/chunk_size/top_k/score_threshold）
DELETE /api/v1/rag/kbs/{kb_id}      # 删除（级联删除文档、chunk 与 Milvus 向量）
```

**DELETE 响应**

```json
{ "code": 0, "data": { "kb_id": 1001, "deleted": true, "async_cleanup": true } }
```

> `async_cleanup: true` 表示 Milvus 向量清理为异步执行。因检索结果最终回 PG 取正文，PG 侧已软删除，**Milvus 残留向量不会出现在检索结果中**，用户无感知。

### 5.4 上传文档

`POST /api/v1/rag/kbs/{kb_id}/documents`

`Content-Type: multipart/form-data`

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| file | file | 是 | 文档文件，≤ 50MB |
| title | string | 否 | 文档标题，不传则取文件名 |
| splitter | string | 否 | 覆盖知识库默认切片策略 |

**支持格式**：`pdf` `docx` `doc` `html` `htm` `xlsx` `xls` `md` `markdown` `txt`

**响应（202 Accepted，异步处理）**

```json
{
  "code": 0,
  "data": {
    "doc_id": 5001,
    "kb_id": 1001,
    "title": "产品手册 v3.2",
    "file_name": "manual-v3.2.pdf",
    "file_type": "pdf",
    "file_size": 2458112,
    "status": 0,
    "duplicated": false,
    "created_at": "2026-10-08T21:00:00+08:00"
  }
}
```

| 字段 | 说明 |
|---|---|
| status | 0 待处理 / 1 解析中 / 2 已完成 / 3 失败 |
| **duplicated** | `true` 表示同库内已有相同 `file_hash` 的文档，**直接复用已有 chunk，不重复向量化** |

> `202 Accepted` 意味着文档已入库但**尚未完成解析**，需轮询 5.5 的详情接口或通过 `documents.status` 判断。

### 5.5 文档列表 / 详情

`GET /api/v1/rag/kbs/{kb_id}/documents?page=1&page_size=20&status=2`

**响应**

```json
{
  "code": 0,
  "data": {
    "list": [
      {
        "doc_id": 5001,
        "title": "产品手册 v3.2",
        "file_type": "pdf",
        "file_size": 2458112,
        "status": 2,
        "chunk_count": 348,
        "error_msg": "",
        "parsed_at": "2026-10-08T21:03:12+08:00",
        "indexed_at": "2026-10-08T21:04:45+08:00",
        "created_at": "2026-10-08T21:00:00+08:00"
      }
    ],
    "total": 12,
    "page": 1,
    "page_size": 20
  }
}
```

`GET /api/v1/rag/kbs/{kb_id}/documents/{doc_id}` — 详情，额外返回 `metadata`、`source_url`。

**解析失败示例**

```json
{
  "code": 0,
  "data": {
    "doc_id": 5002,
    "status": 3,
    "error_msg": "PDF is encrypted; password required",
    "chunk_count": 0
  }
}
```

### 5.6 重试解析 / 删除文档

```http
POST   /api/v1/rag/kbs/{kb_id}/documents/{doc_id}/retry    # 重新解析（status=3 时可用）
DELETE /api/v1/rag/kbs/{kb_id}/documents/{doc_id}          # 删除文档及其 chunk 与向量
```

**重试响应**

```json
{ "code": 0, "data": { "doc_id": 5002, "status": 0, "message": "已重新入队" } }
```

### 5.7 向量检索

`POST /api/v1/rag/kbs/{kb_id}/retrieve`

**请求**

```json
{
  "query": "如何配置多因子认证？",
  "kb_ids": [1],
  "top_k": 5,
  "score_threshold": 0.30,
  "context_window": 1,
  "exclude_doc_ids": []
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| query | string | 是 | 检索问题，1–2000 字符 |
| kb_ids | int[] | 是 | 检索的知识库列表 |
| top_k | int | 否 | 最终返回条数，默认 5 |
| score_threshold | float | 否 | 向量相似度阈值，默认取服务端配置 |
| context_window | int | 否 | 上下文外扩窗口，0 关闭，默认取服务端配置 |
| exclude_doc_ids | int[] | 否 | 排除指定文档 |
| request_id | string | 否 | 链路追踪 ID |

> **混合检索参数不在请求里**：来源开关（`enable_keyword_search` / `enable_web_search`）、
> 融合策略（`merge_strategy`：`rrf` / `vector_only`）与重排参数一样，都是**服务端配置**，
> 见 `configs/config.example.yaml` 的 `rag:` 段。来源与融合属部署策略，客户端不能按请求覆盖。

**响应**

```json
{
  "code": 0,
  "data": {
    "query": "如何配置多因子认证？",
    "hits": [
      {
        "chunk_id": "chk_7f3a9b2c",
        "doc_id": 5001,
        "doc_title": "产品手册 v3.2",
        "seq": 42,
        "content": "多因子认证（MFA）配置步骤：首先进入「安全设置」页面……",
        "score": 0.8734,
        "page_no": 17
      }
    ],
    "total": 5,
    "degraded": false,
    "retrieve_ms": 87,
    "embedding_ms": 46,
    "search_ms": 41
  }
}
```

| 字段 | 说明 |
|---|---|
| score | 融合后的最终得分（RRF 融合分，或重排分/向量相似度，取决于链路） |
| chunk_id | 知识库切片 ID；**网络来源命中以 `web:` 前缀标识**（`doc_id` 为 0，标题来自搜索结果） |
| **degraded** | `true` 表示本次检索**全部来源失败**，看 `degrade_reason` 判断原因 |

> ⚠️ **多来源降级语义（防单源）**：
> 向量 / 关键词 / 网络三来源**并行召回**，任一来源故障**不**置 `degraded`——
> 其他来源仍会产出结果。只有**全部来源**都失败才 `degraded=true`，
> 返回空 `hits` 并带 `all_sources_failed: ...` 原因。
> 各来源的执行与失败明细在调试端点（5.8）的 `stages` 中可见。

**`degrade_reason` 取值**（可能多个，用 `;` 分隔，根因在前）

| 取值 | 触发条件 | 结果 |
|---|---|---|
| `all_sources_failed: ...` | **全部**检索来源（向量/关键词/网络）均失败 | 返回**空结果**并置 `degraded=true` |
| `rerank_unsupported: ...` | 未配置重排模型，或该模型缺 `rerank_path` | **保留融合召回结果**，按融合分排序 |
| `rerank_timeout: ...` | 重排 RPC 超时 | 同上 |
| `rerank_failed: ...` | 重排 Provider 报错 | 同上 |
| `rerank_rejected_all: ...` | 重排成功但**全部候选低于阈值** | 返回**空结果**。这是模型的正常业务判断，**不是故障**，故 `degraded` 仍为 `false` |

> ⚠️ **重排成功时 `score` 是 rerank 相关性分（0–1），不是向量余弦相似度**。
> 两者量纲不同，不可混用同一个阈值判断。

**降级响应示例（全部来源失败）**

```json
{
  "code": 0,
  "data": {
    "hits": [],
    "total": 0,
    "degraded": true,
    "degrade_reason": "all_sources_failed: embedding 失败; 关键词检索失败; 网络检索失败",
    "retrieve_ms": 132
  }
}
```

### 5.8 检索测试（调参用）

`POST /api/v1/rag/kbs/{kb_id}/debug/retrieve`

与 5.7 参数相同，但额外返回**中间过程信息**，用于调参：

```json
{
  "code": 0,
  "data": {
    "stages": [
      { "stage": "multi_source_recall", "ms": 38, "detail": "sources=[vector keyword web]" },
      { "stage": "vector", "ms": 22, "detail": "hits=5" },
      { "stage": "keyword", "ms": 18, "detail": "hits=3" },
      { "stage": "web", "ms": 41, "detail": "hits=2" },
      { "stage": "fuse", "ms": 1, "detail": "strategy=rrf candidates=8" },
      { "stage": "rerank", "ms": 61, "detail": "kept=5/8 threshold=0.30" }
    ],
    "embed_model": "bge-m3",
    "dimension": 1024,
    "milvus_filter": ""
  }
}
```

> 这是**排查「为什么检索不到/为什么结果不相关」的第一入口**。能看到每一阶段的候选集与过滤掉的数量（`filtered_out` 主要来自 `score_threshold`）。

### 5.9 文档切片预览

`GET /api/v1/rag/kbs/{kb_id}/documents/{doc_id}/chunks?page=1&page_size=20`

**响应**

```json
{
  "code": 0,
  "data": {
    "list": [
      {
        "chunk_id": "chk_7f3a9b2c",
        "seq": 42,
        "content": "多因子认证（MFA）配置步骤：……",
        "token_count": 312,
        "char_count": 498,
        "page_no": 17,
        "heading": "3.4 安全设置 / 多因子认证",
        "vec_status": 1
      }
    ],
    "total": 348,
    "page": 1,
    "page_size": 20
  }
}
```

| 字段 | 说明 |
|---|---|
| vec_status | 0 待写入 Milvus / 1 已写入 / 2 写入失败 |
| token_count / char_count | 用于评估切片大小是否合理 |

> **切片质量是 RAG 效果的上限**。本接口用于人工抽查切片边界是否合理（是否切断了语义、是否包含足够上下文）。若发现切片过碎或过长，应调整 `chunk_size` / `splitter` 后重建。

### 5.10 Agent 挂载 / 卸载知识库

```http
POST   /api/v1/rag/agents/{agent_id}/kbs   # 挂载
DELETE /api/v1/rag/agents/{agent_id}/kbs/{kb_id}  # 卸载
```

**挂载请求**

```json
{ "kb_ids": [1001, 1002] }
```

**响应**

```json
{ "code": 0, "data": { "agent_id": "agent_abc", "kb_ids": [1001, 1002] } }
```

> 挂载关系存储在 `agents.kb_ids`（PostgreSQL `BIGINT[]` 数组）。对话时 `agent_service` 读取该字段，作为检索时的 `kb_id` 白名单。

---

## 6. 系统接口

### 6.1 健康检查

`GET /healthz` — 每服务暴露

```json
{ "status": "ok", "service": "gateway", "version": "1.0.0", "uptime": 3600 }
```

### 6.2 就绪检查

`GET /readyz` — K8s readinessProbe 使用

```json
{ "status": "ready", "deps": { "nacos": "ok", "postgres": "ok", "redis": "ok" } }
```

**rag_service 的就绪检查**（额外包含 Milvus）

```json
{ "status": "ready", "deps": { "nacos": "ok", "postgres": "ok", "redis": "ok", "milvus": "ok", "llm_service": "ok" } }
```

> `milvus` 为 `degraded` 时服务仍返回 `ready`（因为可降级工作），但会在字段中标注。**只有 Milvus 与 PG 同时不可用才返回 `not ready`。**

---

## 7. IDL 定义（Protobuf）

### 7.1 `idl/authv1/authv1.proto`

```proto
syntax = "proto3";
package auth.v1;
option go_package = "github.com/aegis/idl/kitex_gen/authv1";

service AuthService {
  rpc Register(RegisterReq) returns (RegisterResp);
  rpc Login(LoginReq) returns (LoginResp);
  rpc RefreshToken(RefreshTokenReq) returns (LoginResp);
  rpc Logout(LogoutReq) returns (LogoutResp);
  rpc VerifyToken(VerifyTokenReq) returns (VerifyTokenResp);
  rpc IssueAPIKey(IssueAPIKeyReq) returns (IssueAPIKeyResp);
}

message RegisterReq {
  string username = 1;
  string password = 2;
  string email    = 3;
  string phone    = 4;
}
message RegisterResp { string uid = 1; }

message LoginReq {
  string account  = 1;
  string password = 2;
  string device   = 3;
}
message LoginResp {
  string access_token  = 1;
  string refresh_token = 2;
  int64  expires_in    = 3;
  string token_type    = 4;
  UserInfo user        = 5;
}
message UserInfo {
  string uid      = 1;
  string username = 2;
  string nickname = 3;
  repeated string roles = 4;
}

message RefreshTokenReq { string refresh_token = 1; }

message LogoutReq {
  string uid          = 1;
  bool   all_devices  = 2;
}
message LogoutResp { bool success = 1; }

message VerifyTokenReq { string access_token = 1; }
message VerifyTokenResp {
  bool   valid      = 1;
  string uid        = 2;
  repeated string roles = 3;
  string app_id     = 4;
  int64  expires_at = 5;
}

message IssueAPIKeyReq {
  string uid        = 1;
  string name       = 2;
  int64  expire_days = 3;
}
message IssueAPIKeyResp {
  string key_id   = 1;
  string api_key  = 2;
  int64  expires_at = 3;
}
```

### 7.2 `idl/userv1/userv1.proto`

```proto
syntax = "proto3";
package user.v1;
option go_package = "github.com/aegis/idl/kitex_gen/userv1";

service UserService {
  rpc GetProfile(GetProfileReq) returns (GetProfileResp);
  rpc UpdateProfile(UpdateProfileReq) returns (UpdateProfileResp);
  rpc ListUsers(ListUsersReq) returns (ListUsersResp);
  rpc ListRoles(ListRolesReq) returns (ListRolesResp);
  rpc AssignRoles(AssignRolesReq) returns (AssignRolesResp);
  rpc GetQuota(GetQuotaReq) returns (GetQuotaResp);
  rpc UpdateQuotaUsed(UpdateQuotaUsedReq) returns (UpdateQuotaUsedResp);
}

message GetProfileReq  { string uid = 1; }
message GetProfileResp {
  string uid      = 1;
  string username = 2;
  string nickname = 3;
  string avatar   = 4;
  string email    = 5;
  string phone    = 6;
  repeated string roles = 7;
  int32  status   = 8;
  int64  created_at = 9;
  int64  last_login_at = 10;
}

message UpdateProfileReq {
  string uid      = 1;
  string nickname = 2;
  string avatar   = 3;
}
message UpdateProfileResp { bool success = 1; }

message ListUsersReq {
  int32  page     = 1;
  int32  page_size = 2;
  string keyword  = 3;
  int32  status   = 4;
}
message ListUsersResp {
  repeated GetProfileResp list = 1;
  int64 total     = 2;
  int32 page      = 3;
  int32 page_size = 4;
}

message ListRolesReq {}
message ListRolesResp {
  repeated Role list = 1;
}
message Role {
  string role_id = 1;
  string name    = 2;
  string desc    = 3;
}

message AssignRolesReq {
  string uid = 1;
  repeated string role_ids = 2;
}
message AssignRolesResp { bool success = 1; }

message GetQuotaReq { string app_id = 1; }
message GetQuotaResp {
  string app_id     = 1;
  int64  quota_total = 2;
  int64  quota_used  = 3;
  int64  quota_left  = 4;
  int64  reset_at    = 5;
}

message UpdateQuotaUsedReq {
  string app_id = 1;
  int64  tokens = 2;
}
message UpdateQuotaUsedResp { bool success = 1; }
```

### 7.3 `idl/agentv1/agentv1.proto`

```proto
syntax = "proto3";
package agent.v1;
option go_package = "github.com/aegis/idl/kitex_gen/agentv1";

service AgentService {
  rpc CreateSession(CreateSessionReq) returns (CreateSessionResp);
  rpc ListSessions(ListSessionsReq) returns (ListSessionsResp);
  rpc ListMessages(ListMessagesReq) returns (ListMessagesResp);
  rpc DeleteSession(DeleteSessionReq) returns (DeleteSessionResp);
  rpc Chat(ChatReq) returns (ChatResp);
  rpc ChatStream(ChatReq) returns (stream ChatChunk);
  rpc CreateAgent(CreateAgentReq) returns (CreateAgentResp);
  rpc ListAgents(ListAgentsReq) returns (ListAgentsResp);
  rpc GetAgent(GetAgentReq) returns (GetAgentResp);
  rpc UpdateAgent(UpdateAgentReq) returns (UpdateAgentResp);
  rpc DeleteAgent(DeleteAgentReq) returns (DeleteAgentResp);
}

message CreateSessionReq {
  string uid      = 1;
  string agent_id = 2;
  string title    = 3;
  map<string,string> metadata = 4;
}
message CreateSessionResp {
  string session_id = 1;
  string agent_id   = 2;
  string title      = 3;
  int64  created_at = 4;
}

message ListSessionsReq {
  string uid    = 1;
  int32  page   = 2;
  int32  page_size = 3;
}
message SessionInfo {
  string session_id = 1;
  string agent_id   = 2;
  string title      = 3;
  int32  message_count = 4;
  int64  last_message_at = 5;
}
message ListSessionsResp {
  repeated SessionInfo list = 1;
  int64 total = 2;
  int32 page  = 3;
  int32 page_size = 4;
}

message ListMessagesReq {
  string session_id = 1;
  int32  page       = 2;
  int32  page_size  = 3;
}
message MessageInfo {
  string msg_id   = 1;
  string role     = 2;
  string content  = 3;
  int32  tokens   = 4;
  int64  created_at = 5;
}
message ListMessagesResp {
  repeated MessageInfo list = 1;
  int64 total = 2;
  int32 page  = 3;
  int32 page_size = 4;
}

message DeleteSessionReq { string session_id = 1; string uid = 2; }
message DeleteSessionResp { bool success = 1; }

message ChatReq {
  string uid        = 1;
  string session_id = 2;
  string agent_id   = 3;
  string message    = 4;
  string model      = 5;
  float  temperature = 6;
  int32  max_tokens  = 7;
  repeated string tools = 8;
}
message ChatResp {
  string session_id  = 1;
  string msg_id      = 2;
  string content     = 3;
  string model       = 4;
  string finish_reason = 5;
  Usage  usage       = 6;
  int64  latency_ms  = 7;
  repeated ToolCallInfo tool_calls = 8;
}
message Usage {
  int32 prompt_tokens     = 1;
  int32 completion_tokens = 2;
  int32 total_tokens      = 3;
}
message ToolCallInfo {
  string tool   = 1;
  string status = 2;
  int64  duration_ms = 3;
}

message ChatChunk {
  string type      = 1;   // start | delta | tool_call | usage | end | error
  string content   = 2;
  string session_id = 3;
  string msg_id    = 4;
  string model     = 5;
  Usage  usage     = 6;
  string finish_reason = 7;
  string error_message = 8;
}

message CreateAgentReq {
  string uid           = 1;
  string name          = 2;
  string description   = 3;
  string system_prompt = 4;
  string model         = 5;
  float  temperature   = 6;
  int32  max_tokens    = 7;
  repeated string tools = 8;
  int32  context_rounds = 9;
}
message CreateAgentResp {
  string agent_id   = 1;
  int64  created_at = 2;
}

message ListAgentsReq { string uid = 1; int32 page = 2; int32 page_size = 3; }
message AgentInfo {
  string agent_id    = 1;
  string name        = 2;
  string description = 3;
  string model       = 4;
  int64  created_at  = 5;
}
message ListAgentsResp {
  repeated AgentInfo list = 1;
  int64 total = 2;
  int32 page  = 3;
  int32 page_size = 4;
}

message GetAgentReq  { string agent_id = 1; }
message GetAgentResp {
  string agent_id      = 1;
  string name          = 2;
  string description   = 3;
  string system_prompt = 4;
  string model         = 5;
  float  temperature   = 6;
  int32  max_tokens    = 7;
  repeated string tools = 8;
  int32  context_rounds = 9;
}

message UpdateAgentReq {
  string agent_id      = 1;
  string name          = 2;
  string description   = 3;
  string system_prompt = 4;
  string model         = 5;
  float  temperature   = 6;
  int32  max_tokens    = 7;
  repeated string tools = 8;
  int32  context_rounds = 9;
}
message UpdateAgentResp { bool success = 1; }

message DeleteAgentReq { string agent_id = 1; string uid = 2; }
message DeleteAgentResp { bool success = 1; }
```

### 7.4 `idl/llmv1/llmv1.proto`

```proto
syntax = "proto3";
package llm.v1;
option go_package = "github.com/aegis/idl/kitex_gen/llmv1";

service LLMService {
  rpc ListModels(ListModelsReq) returns (ListModelsResp);
  rpc Chat(ChatReq) returns (ChatResp);
  rpc ChatStream(ChatReq) returns (stream ChatChunk);
  rpc GetUsage(GetUsageReq) returns (GetUsageResp);
  rpc RenderPrompt(RenderPromptReq) returns (RenderPromptResp);
}

message ListModelsReq {}
message ModelInfo {
  string name           = 1;
  string provider       = 2;
  int32  context_window = 3;
  int32  max_output     = 4;
  bool   supports_tools = 5;
  bool   supports_stream = 6;
  double price_in       = 7;
  double price_out      = 8;
  string status         = 9;
}
message ListModelsResp { repeated ModelInfo list = 1; }

message Message {
  string role    = 1;   // system | user | assistant | tool
  string content = 2;
  string tool_call_id = 3;
  repeated ToolCall tool_calls = 4;
}
message ToolCall {
  string id        = 1;
  string name      = 2;
  string arguments = 3;
}
message ToolSpec {
  string name        = 1;
  string description = 2;
  string params_schema = 3;   // JSON Schema 字符串
}

message ChatReq {
  string app_id      = 1;
  string uid         = 2;
  string model       = 3;
  repeated Message messages = 4;
  float  temperature = 5;
  int32  max_tokens  = 6;
  repeated ToolSpec tools = 7;
  string tool_choice = 8;
}
message ChatResp {
  string model       = 1;
  Message message    = 2;
  repeated ToolCall tool_calls = 3;
  string finish_reason = 4;
  Usage  usage       = 5;
  string fallback_from = 6;
  int64  latency_ms  = 7;
}
message Usage {
  int32 prompt_tokens     = 1;
  int32 completion_tokens = 2;
  int32 total_tokens      = 3;
}
message ChatChunk {
  string type    = 1;   // start | delta | tool_call | usage | end | error
  string content = 2;
  string model   = 3;
  Usage  usage   = 4;
  string finish_reason = 5;
  string error_message = 6;
}

message GetUsageReq {
  string app_id = 1;
  string start  = 2;   // YYYY-MM-DD
  string end    = 3;
  string granularity = 4;  // day | hour
}
message UsageBucket {
  string date = 1;
  int64  prompt_tokens = 2;
  int64  completion_tokens = 3;
  double cost = 4;
}
message GetUsageResp {
  Usage summary = 1;
  double cost   = 2;
  repeated UsageBucket buckets = 3;
}

message RenderPromptReq {
  string template_id = 1;
  map<string,string> variables = 2;
}
message RenderPromptResp { string content = 1; }
```

### 7.5 `idl/ragv1/ragv1.proto`

```proto
syntax = "proto3";
package rag.v1;
option go_package = "github.com/aegis/idl/kitex_gen/ragv1";

service RagService {
  // ---------- 知识库 ----------
  rpc CreateKB(CreateKBReq) returns (CreateKBResp);
  rpc ListKB(ListKBReq) returns (ListKBResp);
  rpc GetKB(GetKBReq) returns (KB);
  rpc UpdateKB(UpdateKBReq) returns (KB);
  rpc DeleteKB(DeleteKBReq) returns (DeleteKBResp);

  // ---------- 文档 ----------
  rpc UploadDocument(UploadDocumentReq) returns (Document);
  rpc ListDocuments(ListDocumentsReq) returns (ListDocumentsResp);
  rpc GetDocument(GetDocumentReq) returns (Document);
  rpc RetryDocument(RetryDocumentReq) returns (Document);
  rpc DeleteDocument(DeleteDocumentReq) returns (DeleteDocumentResp);
  rpc ListChunks(ListChunksReq) returns (ListChunksResp);

  // ---------- 检索 ----------
  rpc Retrieve(RetrieveReq) returns (RetrieveResp);
  rpc DebugRetrieve(DebugRetrieveReq) returns (DebugRetrieveResp);

  // ---------- Agent 挂载 ----------
  rpc BindAgentKB(BindAgentKBReq) returns (BindAgentKBResp);
  rpc UnbindAgentKB(UnbindAgentKBReq) returns (BindAgentKBResp);
}

// ==================== 知识库 ====================

message KB {
  int64  kb_id           = 1;
  int64  uid             = 2;
  string name            = 3;
  string description     = 4;
  int32  visibility      = 5;   // 1私有 2团队 3公开
  string embedding_model = 6;
  int32  embedding_dim   = 7;   // 创建后不可改
  int32  chunk_size      = 8;
  int32  chunk_overlap   = 9;
  string splitter        = 10;  // recursive|markdown|semantic
  int32  top_k           = 11;
  double score_threshold = 12;
  int32  doc_count       = 13;
  int32  chunk_count     = 14;
  int32  status          = 15;
  int64  created_at      = 16;  // Unix 毫秒
  int64  updated_at      = 17;
}

message CreateKBReq {
  int64  uid             = 1;
  string name            = 2;
  string description     = 3;
  int32  visibility      = 4;
  string embedding_model = 5;
  int32  chunk_size      = 6;
  int32  chunk_overlap   = 7;
  string splitter        = 8;
  int32  top_k           = 9;
  double score_threshold = 10;
}
message CreateKBResp { KB kb = 1; }

message ListKBReq {
  int64  uid       = 1;
  int32  page      = 2;
  int32  page_size = 3;
  string keyword   = 4;
}
message ListKBResp {
  repeated KB list = 1;
  int64 total      = 2;
  int32 page       = 3;
  int32 page_size  = 4;
}

message GetKBReq { int64 kb_id = 1; int64 uid = 2; }

message UpdateKBReq {
  int64  kb_id           = 1;
  int64  uid             = 2;
  string name            = 3;
  string description     = 4;
  int32  visibility      = 5;
  int32  chunk_size      = 6;
  int32  chunk_overlap   = 7;
  int32  top_k           = 8;
  double score_threshold = 9;
}

message DeleteKBReq  { int64 kb_id = 1; int64 uid = 2; }
message DeleteKBResp { int64 kb_id = 1; bool deleted = 2; bool async_cleanup = 3; }

// ==================== 文档 ====================

message Document {
  int64  doc_id      = 1;
  int64  kb_id       = 2;
  int64  uid         = 3;
  string title       = 4;
  string file_name   = 5;
  string file_type   = 6;
  int64  file_size   = 7;
  string file_hash   = 8;
  int32  status      = 9;   // 0待处理 1解析中 2已完成 3失败
  int32  chunk_count = 10;
  string error_msg   = 11;
  bool   duplicated  = 12;  // 是否为去重命中（复用已有 chunk）
  int64  parsed_at   = 13;
  int64  indexed_at  = 14;
  int64  created_at  = 15;
}

message UploadDocumentReq {
  int64  kb_id    = 1;
  int64  uid      = 2;
  string title    = 3;
  string file_name= 4;
  string file_type= 5;
  int64  file_size= 6;
  bytes  content  = 7;      // 原始文件字节流（大文件建议走对象存储直传）
  string splitter = 8;      // 可选，覆盖知识库默认切片策略
}

message ListDocumentsReq {
  int64  kb_id     = 1;
  int64  uid       = 2;
  int32  page      = 3;
  int32  page_size = 4;
  int32  status    = 5;     // -1 表示不过滤
}
message ListDocumentsResp {
  repeated Document list = 1;
  int64 total            = 2;
  int32 page             = 3;
  int32 page_size        = 4;
}

message GetDocumentReq      { int64 kb_id = 1; int64 doc_id = 2; int64 uid = 3; }
message RetryDocumentReq    { int64 kb_id = 1; int64 doc_id = 2; int64 uid = 3; }
message DeleteDocumentReq   { int64 kb_id = 1; int64 doc_id = 2; int64 uid = 3; }
message DeleteDocumentResp  { int64 doc_id = 1; bool deleted = 2; bool async_cleanup = 3; }

message Chunk {
  string chunk_id    = 1;
  int64  doc_id      = 2;
  int64  kb_id       = 3;
  int32  seq         = 4;
  string content     = 5;
  int32  token_count = 6;
  int32  char_count  = 7;
  int32  page_no     = 8;
  string heading     = 9;
  int32  vec_status  = 10;  // 0待写 1已写 2失败
}

message ListChunksReq {
  int64 kb_id     = 1;
  int64 doc_id    = 2;
  int64 uid       = 3;
  int32 page      = 4;
  int32 page_size = 5;
}
message ListChunksResp {
  repeated Chunk list = 1;
  int64 total         = 2;
  int32 page          = 3;
  int32 page_size     = 4;
}

// ==================== 检索 ====================

message RetrieveReq {
  string query      = 1;
  repeated int64 kb_ids = 2;
  int32  top_k      = 3;          // 默认 5
  double score_threshold = 4;
  int32  context_window = 5;      // 上下文外扩
  common.v1.Identity identity = 6;
  string request_id = 7;
  repeated int64 exclude_doc_ids = 8;
}

message Hit {
  string chunk_id      = 1;
  int64  doc_id        = 2;
  string doc_title     = 3;
  int32  seq           = 4;
  string content       = 5;
  double score         = 6;
  double vector_score  = 7;
  double keyword_score = 8;
  int32  page_no       = 9;
  string heading       = 10;
  int32  start_offset  = 11;
  int32  end_offset    = 12;
}

message RetrieveResp {
  string query         = 1;
  repeated Hit hits    = 2;
  int32  total         = 3;
  bool   degraded      = 4;   // ★ true 表示有降级环节，见 degrade_reason
  string degrade_reason= 5;
  int64  retrieve_ms   = 6;
  int64  embedding_ms  = 7;
  int64  search_ms     = 8;
}

message DebugRetrieveReq { RetrieveReq base = 1; }

message DebugRetrieveResp {
  repeated Hit vector_hits     = 1;
  repeated Hit keyword_hits    = 2;
  repeated Hit fused_hits      = 3;
  repeated Hit reranked_hits   = 4;
  int32 filtered_out           = 5;
  map<string,int64> timings    = 6;   // embedding_ms / vector_ms / keyword_ms / rerank_ms
}

// ==================== Agent 挂载 ====================

message BindAgentKBReq {
  string agent_id        = 1;
  int64  uid             = 2;
  repeated int64 kb_ids  = 3;
}
message UnbindAgentKBReq {
  string agent_id = 1;
  int64  uid      = 2;
  int64  kb_id    = 3;
}
message BindAgentKBResp {
  string agent_id        = 1;
  repeated int64 kb_ids  = 2;
}
```

> **设计说明**：`Retrieve` 的鉴权参数 `uid` 显式出现在请求中（而非依赖隐式上下文），是为了**强制在服务边界校验知识库归属**，避免越权检索。服务端会按「`uid` 匹配创建者 或 `visibility` 允许 或 团队共享」判定，并将允许的 `kb_id` 白名单下推到 Milvus 的 `filter`。**绝不信任仅由调用方传入的 `kb_id`。**

### 7.6 代码生成命令

```bash
# 生成 auth_service
bash scripts/gen_proto.sh auth_service idl/authv1/authv1.proto

# 生成 user_service
bash scripts/gen_proto.sh user_service idl/userv1/userv1.proto

# 生成 agent_service
bash scripts/gen_proto.sh agent_service idl/agentv1/agentv1.proto

# 生成 llm_service
bash scripts/gen_proto.sh llm_service idl/llmv1/llmv1.proto

# 生成 rag_service（新增）
bash scripts/gen_proto.sh rag_service idl/ragv1/ragv1.proto
```

> 生成后需将 `kitex_gen/` 目录纳入版本控制，`handler.go` 内的业务逻辑不会被覆盖。

---

## 8. 接口鉴权矩阵

| 接口 | 匿名 | 普通用户 | 管理员 | 服务间 |
|---|---|---|---|---|
| `POST /auth/register` | ✅ | — | — | — |
| `POST /auth/login` | ✅ | — | — | — |
| `POST /auth/refresh` | ✅ | — | — | — |
| `POST /auth/logout` | — | ✅ | ✅ | — |
| `POST /auth/verify` | — | — | — | ✅ |
| `GET /user/profile` | — | ✅ | ✅ | — |
| `GET /user/list` | — | — | ✅ | — |
| `POST /user/roles/assign` | — | — | ✅ | — |
| `GET /user/quota` | — | ✅ | ✅ | — |
| `POST /agent/session/create` | — | ✅ | ✅ | — |
| `POST /agent/chat` | — | ✅ | ✅ | — |
| `POST /agent/chat/stream` | — | ✅ | ✅ | — |
| `POST /agent/create` | — | — | ✅ | — |
| `GET /llm/models` | — | ✅ | ✅ | ✅ |
| `POST /llm/chat` | — | ✅ | ✅ | ✅ |
| `GET /llm/usage` | — | ✅ | ✅ | — |
| `POST /llm/embeddings` | — | — | — | ✅ |
| **`POST /rag/kbs`** | — | ✅ `kb:create` | ✅ | — |
| **`GET /rag/kbs`** | — | ✅ `kb:read` | ✅ | — |
| **`GET /rag/kbs/{id}`** | — | ✅ `kb:read` | ✅ | — |
| **`PUT /rag/kbs/{id}`** | — | ✅ `kb:write` | ✅ | — |
| **`DELETE /rag/kbs/{id}`** | — | ✅ `kb:delete` | ✅ | — |
| **`POST /rag/kbs/{id}/documents`** | — | ✅ `kb:write` | ✅ | — |
| **`GET /rag/kbs/{id}/documents`** | — | ✅ `kb:read` | ✅ | — |
| **`GET /rag/kbs/{id}/documents/{did}`** | — | ✅ `kb:read` | ✅ | — |
| **`POST /rag/kbs/{id}/documents/{did}/retry`** | — | ✅ `kb:write` | ✅ | — |
| **`DELETE /rag/kbs/{id}/documents/{did}`** | — | ✅ `kb:write` | ✅ | — |
| **`GET /rag/kbs/{id}/documents/{did}/chunks`** | — | ✅ `kb:read` | ✅ | — |
| **`POST /rag/kbs/{id}/retrieve`** | — | ✅ `kb:retrieve` | ✅ | ✅ |
| **`POST /rag/kbs/{id}/debug/retrieve`** | — | ✅ `kb:retrieve` | ✅ | — |
| **`POST /rag/agents/{aid}/kbs`** | — | ✅ `kb:write` | ✅ | — |
| **`DELETE /rag/agents/{aid}/kbs/{kid}`** | — | ✅ `kb:write` | ✅ | — |
| `GET /healthz` | ✅ | ✅ | ✅ | ✅ |
