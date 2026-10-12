// 各业务域 API（与 gateway 路由一一对应）
import http, { getToken } from './client'
import type {
  Agent, Session, ChatMessage, Citation,
  KnowledgeBase, Doc, Chunk, Hit,
  ModelInfo, Capabilities, UsageData,
  User, LoginData, PageMeta,
} from '../types'

const unwrap = <T>(r: { data: { data: T } }): T => r.data.data

// ---------- 认证 ----------
export const authApi = {
  login: (username: string, password: string) =>
    http.post<{ data: LoginData }>('/api/v1/auth/login', { username, password }).then(unwrap),
  me: () => http.get<{ data: LoginData['user'] }>('/api/v1/auth/me').then(unwrap),
}

// ---------- Agent ----------
export const agentApi = {
  list: (params: { page?: number; page_size?: number; keyword?: string } = {}) =>
    http.get<{ data: { agents: Agent[]; meta: PageMeta } }>('/api/v1/agents', { params }).then(unwrap),
  create: (data: Partial<Agent>) =>
    http.post<{ data: { agent_id: number } }>('/api/v1/agents', data).then(unwrap),
  update: (id: number, data: Partial<Agent>) =>
    http.put<{ data: Agent }>(`/api/v1/agents/${id}`, data).then(unwrap),
  remove: (id: number) => http.delete(`/api/v1/agents/${id}`),
  sessions: (agentId: number) =>
    http.get<{ data: { sessions: Session[]; meta: PageMeta } }>(`/api/v1/sessions`, { params: { agent_id: agentId } }).then(unwrap),
  messages: (sessionId: number) =>
    http.get<{ data: { messages: ChatMessage[]; meta: PageMeta } }>(
      `/api/v1/sessions/${sessionId}/messages`, { params: { direction: 1 } },
    ).then(unwrap),
  bindKB: (agentId: number, kbIds: number[]) =>
    http.post(`/api/v1/agents/${agentId}/knowledge-bases/bind`, { kb_ids: kbIds }),
  listKB: (agentId: number) =>
    http.get<{ data: { kbs: KnowledgeBase[] } }>(`/api/v1/agents/${agentId}/knowledge-bases`).then(unwrap),
}

// ---------- 知识库 ----------
export const kbApi = {
  list: (params: { page?: number; page_size?: number; keyword?: string } = {}) =>
    http.get<{ data: { kbs: KnowledgeBase[]; meta: PageMeta } }>('/api/v1/knowledge-bases', { params }).then(unwrap),
  create: (data: Partial<KnowledgeBase>) =>
    http.post<{ data: { kb_id: number } }>('/api/v1/knowledge-bases', data).then(unwrap),
  update: (id: number, data: Partial<KnowledgeBase>) =>
    http.put<{ data: KnowledgeBase }>(`/api/v1/knowledge-bases/${id}`, data).then(unwrap),
  remove: (id: number) => http.delete(`/api/v1/knowledge-bases/${id}`),
  docs: (kbId: number, params: { page?: number; page_size?: number } = {}) =>
    http.get<{ data: { docs: Doc[]; meta: PageMeta } }>(`/api/v1/knowledge-bases/${kbId}/documents`, { params }).then(unwrap),
  upload: (kbId: number, file: File, fileType: string) => {
    const fd = new FormData()
    fd.append('file', file)
    fd.append('file_type', fileType)
    fd.append('file_name', file.name)
    return http.post<{ data: { doc_id: number; message: string; status: number; duplicated: boolean } }>(
      `/api/v1/knowledge-bases/${kbId}/documents`, fd,
      { headers: { 'Content-Type': 'multipart/form-data' } },
    ).then(unwrap)
  },
  removeDoc: (docId: number) => http.delete(`/api/v1/knowledge-bases/documents/${docId}`),
  retryDoc: (docId: number) => http.post(`/api/v1/knowledge-bases/documents/${docId}/retry`, {}),
  chunks: (docId: number) =>
    http.get<{ data: { chunks: Chunk[] } }>(`/api/v1/knowledge-bases/documents/${docId}/chunks`).then(unwrap),
  retrieve: (data: { kb_ids: number[]; query: string; top_k?: number }) =>
    http.post<{ data: { hits: Hit[]; total: number; degraded: boolean; degrade_reason?: string } }>(
      '/api/v1/rag/retrieve', data,
    ).then(unwrap),
}

// ---------- LLM ----------
export const llmApi = {
  models: () =>
    http.get<{ data: { models: ModelInfo[]; default_model: string } }>('/api/v1/llm/models').then(unwrap),
  capabilities: () =>
    http.get<{ data: Capabilities }>('/api/v1/llm/capabilities').then(unwrap),
  usage: (params: { page?: number; page_size?: number } = {}) =>
    http.get<{ data: UsageData }>('/api/v1/llm/usage', { params }).then(unwrap),
  rerank: (data: { query: string; documents: string[]; top_n?: number }) =>
    http.post<{ data: unknown }>('/api/v1/llm/rerank', data).then(unwrap),
}

// ---------- 用户 ----------
export const userApi = {
  list: (params: { page?: number; page_size?: number; keyword?: string } = {}) =>
    http.get<{ data: { users: User[]; meta: PageMeta } }>('/api/v1/users', { params }).then(unwrap),
  create: (data: { username: string; password: string; nickname?: string; email?: string; roles?: string[] }) =>
    http.post<{ data: { uid: number; message?: string } }>('/api/v1/users', data).then(unwrap),
  update: (uid: number, data: Partial<User>) =>
    http.put<{ data: User }>(`/api/v1/users/${uid}`, data).then(unwrap),
  remove: (uid: number) => http.delete(`/api/v1/users/${uid}`),
  setPreference: (items: Record<string, string>) =>
    http.put('/api/v1/users/me/preferences', { items }),
  getPreference: () =>
    http.get<{ data: { items: Record<string, string> } }>('/api/v1/users/me/preferences').then(unwrap),
}

// ---------- SSE 流式对话（POST，支持 body + headers，可携带 token）----------
export async function streamChat(
  agentId: number,
  body: { content: string; session_id?: number; use_rag?: boolean; kb_ids?: number[] },
  onEvent: (ev: { type: string; [k: string]: unknown }) => void,
  signal?: AbortSignal,
): Promise<number | undefined> {
  const resp = await fetch(`/api/v1/agents/${agentId}/chat/stream`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${getToken()}`,
    },
    body: JSON.stringify(body),
    signal,
  })
  if (!resp.ok || !resp.body) {
    const err = await resp.json().catch(() => null)
    throw new Error(err?.error?.message || `请求失败(${resp.status})`)
  }

  const reader = resp.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  let sessionId: number | undefined

  const parseEvent = (block: string) => {
    const dataLine = block.split('\n').find((l) => l.startsWith('data:'))
    if (!dataLine) return
    try {
      const ev = JSON.parse(dataLine.slice(5).trim()) as { type: string; [k: string]: unknown }
      if (ev.session_id) sessionId = ev.session_id as number
      onEvent(ev)
    } catch {
      /* 忽略无法解析的帧 */
    }
  }

  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    // SSE 帧以空行分隔
    let idx: number
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const block = buf.slice(0, idx)
      buf = buf.slice(idx + 2)
      if (block.trim()) parseEvent(block)
    }
  }
  return sessionId
}
