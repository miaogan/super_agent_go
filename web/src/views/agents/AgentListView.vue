<template>
  <div>
    <el-card shadow="never">
      <div class="toolbar">
        <el-input
          v-model="keyword" placeholder="搜索智能体" clearable style="width: 240px"
          @keyup.enter="load" @clear="load"
        />
        <el-button type="primary" @click="openCreate">新建智能体</el-button>
      </div>

      <el-table :data="list" v-loading="loading" stripe>
        <el-table-column prop="agent_id" label="ID" width="70" />
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column prop="description" label="描述" min-width="220" show-overflow-tooltip />
        <el-table-column prop="model" label="模型" width="130" />
        <el-table-column label="RAG" width="80">
          <template #default="{ row }">
            <el-tag v-if="row.rag_enabled" type="success" size="small">已开启</el-tag>
            <el-tag v-else type="info" size="small">关闭</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="created_at" label="创建时间" width="170">
          <template #default="{ row }">{{ fmtTime(row.created_at) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="200" fixed="right">
          <template #default="{ row }">
            <el-button type="primary" link @click="goChat(row)">对话</el-button>
            <el-button link @click="openEdit(row)">编辑</el-button>
            <el-popconfirm title="确认删除该智能体？" @confirm="remove(row)">
              <template #reference>
                <el-button type="danger" link>删除</el-button>
              </template>
            </el-popconfirm>
          </template>
        </el-table-column>
      </el-table>

      <el-pagination
        v-model:current-page="page" :total="total" :page-size="pageSize"
        layout="total, prev, pager, next" class="pager" @current-change="load"
      />
    </el-card>

    <!-- 创建/编辑对话框 -->
    <el-dialog v-model="dlg" :title="editing ? '编辑智能体' : '新建智能体'" width="560px">
      <el-form :model="form" label-width="110px">
        <el-form-item label="名称" required>
          <el-input v-model="form.name" placeholder="如：客服助手" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.description" type="textarea" :rows="2" />
        </el-form-item>
        <el-form-item label="模型">
          <el-select v-model="form.model" clearable placeholder="默认模型">
            <el-option v-for="m in modelOptions" :key="m" :label="m" :value="m" />
          </el-select>
        </el-form-item>
        <el-form-item label="系统提示词">
          <el-input v-model="form.system_prompt" type="textarea" :rows="3" />
        </el-form-item>
        <el-form-item label="温度">
          <el-slider v-model="form.temperature" :min="0" :max="1" :step="0.1" show-input />
        </el-form-item>
        <el-form-item label="最大 Tokens">
          <el-input-number v-model="form.max_tokens" :min="64" :max="8192" :step="64" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dlg = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { agentApi, llmApi } from '../../api'
import type { Agent } from '../../types'

const router = useRouter()
const list = ref<Agent[]>([])
const loading = ref(false)
const keyword = ref('')
const page = ref(1)
const pageSize = 10
const total = ref(0)

const dlg = ref(false)
const editing = ref<Agent | null>(null)
const saving = ref(false)
const modelOptions = ref<string[]>([])
const form = reactive({
  name: '', description: '', model: '', system_prompt: '',
  temperature: 0.7, max_tokens: 2048,
})

function fmtTime(ts: number) {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString('zh-CN')
}

async function load() {
  loading.value = true
  try {
    const data = await agentApi.list({ page: page.value, page_size: pageSize, keyword: keyword.value || undefined })
    list.value = data.agents
    total.value = data.meta?.total || 0
  } finally {
    loading.value = false
  }
}

function goChat(row: Agent) {
  router.push(`/agents/${row.agent_id}/chat`)
}

function openCreate() {
  editing.value = null
  Object.assign(form, { name: '', description: '', model: '', system_prompt: '', temperature: 0.7, max_tokens: 2048 })
  dlg.value = true
}

function openEdit(row: Agent) {
  editing.value = row
  Object.assign(form, {
    name: row.name, description: row.description || '', model: row.model || '',
    system_prompt: row.system_prompt || '', temperature: row.temperature, max_tokens: row.max_tokens,
  })
  dlg.value = true
}

async function save() {
  if (!form.name) {
    ElMessage.warning('名称不能为空')
    return
  }
  saving.value = true
  try {
    if (editing.value) {
      await agentApi.update(editing.value.agent_id, { ...form })
    } else {
      await agentApi.create({ ...form })
    }
    ElMessage.success('保存成功')
    dlg.value = false
    load()
  } finally {
    saving.value = false
  }
}

async function remove(row: Agent) {
  await agentApi.remove(row.agent_id)
  ElMessage.success('已删除')
  load()
}

onMounted(async () => {
  load()
  try {
    const data = await llmApi.models()
    modelOptions.value = data.models.filter((m) => m.status === 'enabled').map((m) => m.name)
  } catch {
    /* 模型列表拉取失败不阻塞页面 */
  }
})
</script>

<style scoped>
.toolbar { display: flex; justify-content: space-between; margin-bottom: 14px; }
.pager { margin-top: 14px; justify-content: flex-end; }
</style>
