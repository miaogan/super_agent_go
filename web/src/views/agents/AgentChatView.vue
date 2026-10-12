<template>
  <el-card shadow="never" class="chat-card" body-style="height: calc(100vh - 140px); padding: 0; display: flex">
    <!-- 会话列表 -->
    <div class="sessions">
      <el-button class="new-session" type="primary" plain size="small" @click="newSession">＋ 新对话</el-button>
      <div
        v-for="s in sessions" :key="s.session_id"
        class="session-item" :class="{ active: s.session_id === currentSession }"
        @click="switchSession(s.session_id)"
      >
        {{ s.title || `会话 ${s.session_id}` }}
      </div>
      <el-empty v-if="!sessions.length" description="暂无会话" :image-size="60" />
    </div>

    <!-- 消息区 -->
    <div class="chat-main">
      <div ref="scrollRef" class="messages">
        <el-empty v-if="!messages.length" description="开始一段对话吧" />
        <div v-for="(m, i) in messages" :key="i" class="msg" :class="m.role">
          <div class="avatar">{{ m.role === 'user' ? '我' : 'AI' }}</div>
          <div class="bubble">
            <div class="content">{{ m.content || '…' }}</div>
            <div v-if="m.citations && m.citations.length" class="cites">
              <el-collapse>
                <el-collapse-item :title="`引用 ${m.citations.length} 条`" name="c">
                  <div v-for="(c, ci) in m.citations" :key="ci" class="cite">
                    <span class="cite-tag">{{ c.doc_title || '文档' }}</span>
                    <span class="cite-score">{{ (c.score * 100).toFixed(1) }}%</span>
                    <span class="cite-text">{{ c.content }}</span>
                  </div>
                </el-collapse-item>
              </el-collapse>
            </div>
          </div>
        </div>
      </div>

      <!-- 输入区 -->
      <div class="input-area">
        <div v-if="streaming" class="stream-hint">
          <span>正在生成…</span>
          <el-button link type="danger" @click="stop">停止</el-button>
        </div>
        <el-input
          v-model="input" type="textarea" :rows="3" resize="none"
          placeholder="输入消息，Enter 发送 / Shift+Enter 换行"
          :disabled="streaming"
          @keydown.enter.exact.prevent="send"
        />
        <div class="send-row">
          <el-checkbox v-model="useRag">启用知识库检索</el-checkbox>
          <el-button type="primary" :loading="streaming" @click="send">发送</el-button>
        </div>
      </div>
    </div>
  </el-card>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import { agentApi, streamChat } from '../../api'
import type { Agent, ChatMessage, Session } from '../../types'

const route = useRoute()
const agentId = Number(route.params.id)

const agent = ref<Agent | null>(null)
const sessions = ref<Session[]>([])
const currentSession = ref<number>(0)
const messages = ref<ChatMessage[]>([])
const input = ref('')
const streaming = ref(false)
const useRag = ref(false)
const scrollRef = ref<HTMLElement>()
const controller = ref<AbortController>()

const modelLabel = computed(() => agent.value?.model || '')

async function loadAgent() {
  try {
    const data = await agentApi.list({ page: 1, page_size: 100 })
    agent.value = data.agents.find((a) => a.agent_id === agentId) || null
  } catch {
    /* 忽略 */
  }
}

async function loadSessions() {
  const data = await agentApi.sessions(agentId)
  sessions.value = data.sessions || []
}

async function switchSession(id: number) {
  currentSession.value = id
  const data = await agentApi.messages(id)
  messages.value = data.messages || []
  scrollBottom()
}

function newSession() {
  currentSession.value = 0
  messages.value = []
}

async function send() {
  const content = input.value.trim()
  if (!content || streaming.value) return
  input.value = ''
  messages.value.push({ role: 'user', content, created_at: 0 } as ChatMessage)
  const pending: ChatMessage = { role: 'assistant', content: '', citations: [], created_at: 0 } as ChatMessage
  messages.value.push(pending)
  scrollBottom()

  streaming.value = true
  controller.value = new AbortController()
  try {
    const body: { content: string; session_id?: number; use_rag?: boolean } = { content }
    if (currentSession.value) body.session_id = currentSession.value
    if (useRag.value) body.use_rag = true

    const sid = await streamChat(agentId, body, (ev) => {
      if (ev.type === 'session' && ev.session_id) {
        currentSession.value = ev.session_id as number
      } else if (ev.type === 'delta' && ev.delta) {
        pending.content += ev.delta as string
        scrollBottom(false)
      } else if (ev.type === 'citation' && ev.citation) {
        pending.citations!.push(ev.citation as ChatMessage['citations'][number])
      } else if (ev.type === 'done') {
        if (ev.content) pending.content = ev.content as string
        if (ev.citations) pending.citations = ev.citations as ChatMessage['citations']
      } else if (ev.type === 'error') {
        pending.content = `（错误）${ev.message || ev.code || '未知错误'}`
      }
    }, controller.value.signal)

    if (sid) currentSession.value = sid
    loadSessions()
  } catch (e) {
    const err = e as Error
    if (err.name !== 'AbortError') {
      pending.content = pending.content || `（失败）${err.message}`
      ElMessage.error(err.message || '对话失败')
    }
  } finally {
    streaming.value = false
    scrollBottom()
  }
}

function stop() {
  controller.value?.abort()
  streaming.value = false
}

function scrollBottom(force = true) {
  nextTick(() => {
    const el = scrollRef.value
    if (el) {
      el.scrollTop = force ? el.scrollHeight : el.scrollTop + 200
    }
  })
}

onMounted(() => {
  loadAgent()
  loadSessions().catch(() => {})
})
</script>

<style scoped>
.chat-card { height: calc(100vh - 112px); }
.sessions {
  width: 220px; border-right: 1px solid #e4e7ed; padding: 10px; overflow: auto;
  display: flex; flex-direction: column; gap: 6px;
}
.new-session { margin-bottom: 4px; }
.session-item {
  padding: 8px 10px; border-radius: 6px; cursor: pointer; font-size: 13px;
  color: #606266; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.session-item:hover { background: #f5f7fa; }
.session-item.active { background: #ecf5ff; color: #409eff; }
.chat-main { flex: 1; display: flex; flex-direction: column; min-width: 0; }
.messages { flex: 1; overflow: auto; padding: 18px; }
.msg { display: flex; gap: 10px; margin-bottom: 16px; }
.msg.user { flex-direction: row-reverse; }
.avatar {
  width: 34px; height: 34px; border-radius: 50%; flex-shrink: 0;
  display: flex; align-items: center; justify-content: center;
  font-size: 13px; color: #fff;
}
.msg.user .avatar { background: #409eff; }
.msg.assistant .avatar { background: #67c23a; }
.bubble {
  max-width: 72%; padding: 10px 14px; border-radius: 10px; background: #fff;
  box-shadow: 0 1px 3px rgba(0,0,0,.08); font-size: 14px; line-height: 1.7;
  white-space: pre-wrap; word-break: break-word;
}
.msg.user .bubble { background: #ecf5ff; }
.cites { margin-top: 8px; }
.cite { display: flex; gap: 8px; font-size: 12px; color: #606266; margin-bottom: 4px; align-items: baseline; }
.cite-tag { color: #409eff; flex-shrink: 0; }
.cite-score { color: #67c23a; flex-shrink: 0; }
.cite-text { color: #909399; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.input-area { border-top: 1px solid #e4e7ed; padding: 10px 14px; }
.stream-hint { font-size: 13px; color: #67c23a; margin-bottom: 6px; display: flex; justify-content: space-between; }
.send-row { display: flex; justify-content: space-between; align-items: center; margin-top: 8px; }
</style>
