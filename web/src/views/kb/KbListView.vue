<template>
  <div>
    <el-card shadow="never">
      <div class="toolbar">
        <el-input
          v-model="keyword" placeholder="搜索知识库" clearable style="width: 240px"
          @keyup.enter="load" @clear="load"
        />
        <el-button type="primary" @click="dlg = true">新建知识库</el-button>
      </div>

      <el-table :data="list" v-loading="loading" stripe>
        <el-table-column prop="kb_id" label="ID" width="70" />
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column prop="description" label="描述" min-width="220" show-overflow-tooltip />
        <el-table-column label="模型" width="140">
          <template #default="{ row }">{{ row.embed_model }}</template>
        </el-table-column>
        <el-table-column label="维度" width="80">
          <template #default="{ row }">{{ row.embedding_dim }}</template>
        </el-table-column>
        <el-table-column label="文档/切片" width="110">
          <template #default="{ row }">{{ row.doc_count }} / {{ row.chunk_count }}</template>
        </el-table-column>
        <el-table-column label="操作" width="160" fixed="right">
          <template #default="{ row }">
            <el-button type="primary" link @click="router.push(`/kbs/${row.kb_id}`)">详情</el-button>
            <el-popconfirm title="确认删除该知识库？" @confirm="remove(row)">
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

    <!-- 新建知识库 -->
    <el-dialog v-model="dlg" title="新建知识库" width="520px">
      <el-form :model="form" label-width="110px">
        <el-form-item label="名称" required>
          <el-input v-model="form.name" placeholder="如：产品文档库" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.description" type="textarea" :rows="2" />
        </el-form-item>
        <el-form-item label="Embedding">
          <el-select v-model="form.embed_model" placeholder="默认 text-embedding-v3">
            <el-option v-for="m in embedOptions" :key="m" :label="m" :value="m" />
          </el-select>
        </el-form-item>
        <el-form-item label="切片大小">
          <el-input-number v-model="form.chunk_size" :min="100" :max="2000" :step="100" />
        </el-form-item>
        <el-form-item label="切片重叠">
          <el-input-number v-model="form.chunk_overlap" :min="0" :max="500" :step="20" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dlg = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="create">创建</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { kbApi, llmApi } from '../../api'
import type { KnowledgeBase } from '../../types'

const router = useRouter()
const list = ref<KnowledgeBase[]>([])
const loading = ref(false)
const keyword = ref('')
const page = ref(1)
const pageSize = 10
const total = ref(0)

const dlg = ref(false)
const saving = ref(false)
const embedOptions = ref<string[]>([])
const form = reactive({ name: '', description: '', embed_model: '', chunk_size: 800, chunk_overlap: 120 })

async function load() {
  loading.value = true
  try {
    const data = await kbApi.list({ page: page.value, page_size: pageSize, keyword: keyword.value || undefined })
    list.value = data.kbs
    total.value = data.meta?.total || 0
  } finally {
    loading.value = false
  }
}

async function create() {
  if (!form.name) {
    ElMessage.warning('名称不能为空')
    return
  }
  saving.value = true
  try {
    await kbApi.create({ ...form })
    ElMessage.success('创建成功')
    dlg.value = false
    load()
  } finally {
    saving.value = false
  }
}

async function remove(row: KnowledgeBase) {
  await kbApi.remove(row.kb_id)
  ElMessage.success('已删除')
  load()
}

onMounted(async () => {
  load()
  try {
    const data = await llmApi.models()
    embedOptions.value = data.models.filter((m) => m.name.includes('embed')).map((m) => m.name)
    if (!embedOptions.value.length) embedOptions.value = ['text-embedding-v3']
  } catch {
    embedOptions.value = ['text-embedding-v3']
  }
})
</script>

<style scoped>
.toolbar { display: flex; justify-content: space-between; margin-bottom: 14px; }
.pager { margin-top: 14px; justify-content: flex-end; }
</style>
