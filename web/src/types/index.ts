// 与后端 REST API 对应的类型定义（字段与 gateway handler 的 JSON 输出一致）

// ---------- 通用 ----------
export interface PageMeta {
  page: number
  page_size: number
  total: number
}

export interface ApiResp<T = unknown> {
  data: T
  error?: { code: string; message: string }
}

// ---------- 认证 ----------
export interface LoginUser {
  uid: number
  username: string
  nickname: string
  email: string
  roles: string[]
  tenant: string
}

export interface LoginData {
  access_token: string
  refresh_token: string
  token_type: string
  expires_at: number
  user: LoginUser
}

// ---------- Agent ----------
export interface Agent {
  agent_id: number
  name: string
  description: string
  model: string
  system_prompt: string
  temperature: number
  max_tokens: number
  rag_enabled: boolean
  rag_top_k: number
  rag_score_threshold: number
  status: string
  created_at: number
  updated_at: number
}

export interface Session {
  session_id: number
  agent_id: number
  title: string
  created_at: number
  updated_at: number
}

export interface Citation {
  index: number
  chunk_id: string
  doc_title: string
  content: string
  score: number
}

export interface ChatMessage {
  msg_id: number
  session_id: number
  role: string
  content: string
  citations?: Citation[]
  usage?: { prompt_tokens: number; completion_tokens: number; total_tokens: number }
  created_at: number
}

// ---------- 知识库 ----------
export interface KnowledgeBase {
  kb_id: number
  name: string
  description: string
  embed_model: string
  embedding_dim: number
  chunk_size: number
  chunk_overlap: number
  splitter: string
  owner_uid: number
  tenant: string
  doc_count: number
  chunk_count: number
  created_at: number
  updated_at: number
}

export interface Doc {
  doc_id: number
  kb_id: number
  file_name: string
  file_type: string
  file_size: number
  file_hash: string
  status: number // 1 待解析 / 2 就绪 / 3 失败 / 4 部分成功
  error_msg: string
  chunk_count: number
  parsed_chunks: number
  created_at: number
  updated_at: number
}

export interface Chunk {
  chunk_id: string
  kb_id: number
  doc_id: number
  seq: number
  content: string
  page_no: number
  token_count: number
  vec_status: number
}

export interface Hit {
  chunk_id: string
  kb_id: number
  doc_id: number
  doc_title: string
  page_no: number
  score: number
  content: string
}

// ---------- LLM ----------
export interface ModelInfo {
  name: string
  provider: string
  base_url: string
  max_tokens: number
  supports_tool_call: boolean
  supports_vision: boolean
  supports_reasoning: boolean
  status: string
  priority: number
}

export interface Capabilities {
  tool_call: boolean
  reasoning: boolean
  multimodal: boolean
  embeddings: boolean
  provider: string
  is_mock: boolean
}

export interface UsageRecord {
  request_id: string
  uid: number
  model: string
  prompt_tokens: number
  completion_tokens: number
  cost: number
  created_at: number
  degraded: boolean
}

export interface UsageData {
  records: UsageRecord[]
  meta: PageMeta
  total_tokens: number
  total_cost: number
}

// ---------- 用户 ----------
export interface User {
  uid: number
  username: string
  nickname: string
  email: string
  phone: string
  avatar: string
  status: number
  roles: string[]
  tenant: string
  created_at: number
  updated_at: number
  source: string
}

// ---------- SSE 事件 ----------
export interface StreamEvent {
  type: string // session / retrieval / citation / delta / reasoning / done / error
  session_id?: number
  hit_count?: number
  retrieve_ms?: number
  citation?: Citation
  delta?: string
  content?: string
  msg_id?: number
  llm_ms?: number
  total_ms?: number
  usage?: { prompt_tokens: number; completion_tokens: number; total_tokens: number }
  citations?: Citation[]
  degraded?: boolean
  degrade_reason?: string
  code?: string
  message?: string
}
