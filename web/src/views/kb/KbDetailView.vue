<template>
  <div class="kb-detail">
    <el-card shadow="never">
      <div class="head">
        <h3>{{ kb?.name || '知识库详情' }}</h3>
        <el-tag size="small" v-if="kb">{{ kb.embed_model }} · dim={{ kb.embedding_dim }} · chunk={{ kb.chunk_size }}/{{ kb.chunk_overlap }}</el-tag>
      </div>

      <el-tabs v-model="tab">
        <!-- 文档 -->
        <el-tab-pane label="文档" name="docs">
          <el-upload
            :auto-upload="false" :show-file-list="false" :on-change="onFileChange"
            accept=".pdf,.docx,.xlsx,.md,.txt,.html,.csv,.json"
          >
            <el-button type="primary" :loading="uploading">上传文档</el-button>
            <template #tip>
              <div class="up-tip">支持 PDF / DOCX / XLSX / MD / TXT / HTML / CSV / JSON</div>
            </template>
          </el-upload>

          <el-table :data="docs" v-loading="docLoading" stripe style="margin-top: 12px">
            <el-table-column prop="file_name" label="文件名" min-width="180" />
            <el-table-column prop="file_type" label="类型" width="80" />
            <el-table-column label="大小" width="100">
              <template #default="{ row }">{{ (row.file_size / 1024).toFixed(1) }}KB</template>
            </el-table-column>
            <el-table-column label="状态" width="110">
              <template #default="{ row }">
                <el-tag :type="statusType(row.status)" size="small">{{ statusText(row.status) }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="切片" width="110">
              <template #default="{ row }">{{ row.parsed_chunks }}/{{ row.chunk_count }}</template>
            </el-table-column>
            <el-table-column prop="error_msg" label="错误信息" min-width="180" show-overflow-tooltip />
            <el-table-column label="操作" width="200" fixed="right">
              <template #default="{ row }">
                <el-button link type="primary" @click="viewChunks(row)">切片</el-button>
                <el-button v-if="row.status === 3" link @click="retry(row)">重试</el-button>
                <el-popconfirm title="确认删除该文档？" @confirm="removeDoc(row)">
                  <template #reference>
                    <el-button type="danger" link>删除</el-button>
                  </template>
                </el-popconfirm>
              </template>
            </el-table-column>
          </el-table>
        </el-tab-pane>

        <!-- Agent 绑定 -->
        <el-tab-pane label="Agent 绑定" name="bind">
          <el-checkbox-group v-model="boundAgents" class="bind-list">
            <el-checkbox v-for="a in allAgents" :key="a.agent_id" :value="a.agent_id">
              {{ a.name }}（#{{ a.agent_id }}）
            </el-checkbox>
          </el-checkbox-group>
          <el-button type="primary" style="margin-top: 12px" :loading="bindSaving" @click="saveBind">保存绑定</el-button>
        </el-tab-pane>

        <!-- 检索测试 -->
        <el-tab-pane label="检索测试" name="search">
          <div class="search-box">
            <el-input v-model="query" placeholder="输入检索问题" @keyup.enter="doSearch" />
            <el-button type="primary" :loading="searching" @click="doSearch">检索</el-button>
          </div>
          <el-table :data="hits" v-loading="searching" stripe style="margin-top: 12px">
            <el-table-column label="分数" width="90">
              <template #default="{ row }">{{ (row.score * 100).toFixed(1) }}%</template>
            </el-table-column>
            <el-table-column prop="doc_title" label="来源文档" width="160" />
            <el-table-column prop="content" label="内容" min-width="360" show-overflow-tooltip />
          </el-table>
          <div v-if="searchDone" class="search-meta">命中 {{ hits.length }} 条{{ degraded ? '（降级模式）' : '' }}</div>
        </el-tab-pane>
      </el-tabs>
    </el-card>

    <!-- 切片查看 -->
    <el-drawer v-model="chunkDrawer" title="文档切片" size="520px">
      <div v-for="c in chunks" :key="c.chunk_id" class="chunk">
        <div class="chunk-head">
          <span>#{{ c.seq }}</span>
          <el-tag size="small" :type="c.vec_status === 1 ? 'success' : c.vec_status === 2 ? 'danger' : 'info'">
            {{ c.vec_status === 1 ? '已向量化' : c.vec_status === 2 ? '失败' : '待处理' }}
          </el-tag>
        </div>
        <div class="chunk-body">{{ c.content }}</div>
      </div>
    </el-drawer>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage, type UploadFile } from 'element-plus'
import { agentApi, kbApi } from '../../api'
import type { Agent, Chunk, Doc, Hit, KnowledgeBase } from '../../types'

const route = useRoute()
const kbId = Number(route.params.id)

const kb = ref<KnowledgeBase | null>(null)
const tab = ref('docs')
const docs = ref<Doc[]>([])
const docLoading = ref(false)
const uploading = ref(false)

const allAgents = ref<Agent[]>([])
const boundAgents = ref<number[]>([])
const bindSaving = ref(false)

const query = ref('')
const hits = ref<Hit[]>([])
const searching = ref(false)
const searchDone = ref(false)
const degraded = ref(false)

const chunkDrawer = ref(false)
const chunks = ref<Chunk[]>([])

function statusText(s: number) {
  return { 1: '解析中', 2: '就绪', 3: '失败', 4: '部分成功' }[s] || String(s)
}
function statusType(s: number): 'info' | 'success' | 'danger' | 'warning' {
  return { 1: 'info', 2: 'success', 3: 'danger', 4: 'warning' }[s] || 'info'
}

async function load() {
  docLoading.value = true
  try {
    const data = await kbApi.list({ page: 1, page_size: 100 })
    kb.value = data.kbs.find((k) => k.kb_id === kbId) || null
    const d = await kbApi.docs(kbId, { page: 1, page_size: 100 })
    docs.value = d.docs || []
  } finally {
    docLoading.value = false
  }
}

async function onFileChange(f: UploadFile) {
  if (!f.raw) return
  const ext = (f.name.split('.').pop() || 'txt').toLowerCase()
  uploading.value = true
  try {
    const r = await kbApi.upload(kbId, f.raw, ext)
    if (r.duplicated) ElMessage.info('文件内容重复，已跳过')
    else if (r.status === 3) ElMessage.error(`解析失败：${r.message || '未知原因'}`)
    else ElMessage.success(r.message || '上传成功')
    load()
  } catch {
    /* 拦截器已提示 */
  } finally {
    uploading.value = false
  }
}

async function removeDoc(row: Doc) {
  await kbApi.removeDoc(row.doc_id)
  ElMessage.success('已删除')
  load()
}

async function retry(row: Doc) {
  await kbApi.retryDoc(row.doc_id)
  ElMessage.success('已触发重试')
  load()
}

async function viewChunks(row: Doc) {
  const data = await kbApi.chunks(row.doc_id)
  chunks.value = data.chunks || []
  chunkDrawer.value = true
}

// ---------- Agent 绑定 ----------
async function loadBind() {
  const a = await agentApi.list({ page: 1, page_size: 100 })
  allAgents.value = a.agents
  const bound: number[] = []
  for (const agent of a.agents) {
    try {
      const kbs = await agentApi.listKB(agent.agent_id)
      if (kbs.kbs?.some((k) => k.kb_id === kbId)) bound.push(agent.agent_id)
    } catch {
      /* 单个查询失败跳过 */
    }
  }
  boundAgents.value = bound
}

async function saveBind() {
  bindSaving.value = true
  try {
    for (const agent of allAgents.value) {
      const checked = boundAgents.value.includes(agent.agent_id)
      const cur = (await agentApi.listKB(agent.agent_id)).kbs?.map((k) => k.kb_id) || []
      const next = checked
        ? Array.from(new Set([...cur, kbId]))
        : cur.filter((id) => id !== kbId)
      await agentApi.bindKB(agent.agent_id, next)
    }
    ElMessage.success('绑定已保存')
  } finally {
    bindSaving.value = false
  }
}

// ---------- 检索测试 ----------
async function doSearch() {
  if (!query.value.trim()) return
  searching.value = true
  try {
    const r = await kbApi.retrieve({ kb_ids: [kbId], query: query.value.trim(), top_k: 5 })
    hits.value = r.hits || []
    degraded.value = !!r.degraded
    searchDone.value = true
  } finally {
    searching.value = false
  }
}

onMounted(() => {
  load()
  loadBind().catch(() => {})
})
</script>

<style scoped>
.head { display: flex; align-items: center; gap: 12px; margin-bottom: 8px; }
.head h3 { margin: 0; }
.up-tip { font-size: 12px; color: #909399; }
.bind-list { display: flex; flex-direction: column; gap: 10px; }
.search-box { display: flex; gap: 10px; }
.search-meta { margin-top: 8px; font-size: 12px; color: #909399; }
.chunk { border: 1px solid #e4e7ed; border-radius: 6px; padding: 10px; margin-bottom: 10px; }
.chunk-head { display: flex; justify-content: space-between; margin-bottom: 6px; font-size: 12px; color: #909399; }
.chunk-body { font-size: 13px; line-height: 1.6; white-space: pre-wrap; }
</style>
