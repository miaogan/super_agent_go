<template>
  <div>
    <el-row :gutter="14">
      <el-col :span="8">
        <el-card shadow="never">
          <template #header>能力声明</template>
          <el-descriptions :column="1" border>
            <el-descriptions-item label="Provider">{{ cap.provider || '-' }}</el-descriptions-item>
            <el-descriptions-item label="Embedding">
              <el-tag :type="cap.embeddings ? 'success' : 'info'" size="small">{{ cap.embeddings ? '支持' : '不支持' }}</el-tag>
            </el-descriptions-item>
            <el-descriptions-item label="Tool Call">
              <el-tag :type="cap.tool_call ? 'success' : 'info'" size="small">{{ cap.tool_call ? '支持' : '不支持' }}</el-tag>
            </el-descriptions-item>
            <el-descriptions-item label="Reasoning">
              <el-tag :type="cap.reasoning ? 'success' : 'info'" size="small">{{ cap.reasoning ? '支持' : '不支持' }}</el-tag>
            </el-descriptions-item>
            <el-descriptions-item label="多模态">
              <el-tag :type="cap.multimodal ? 'success' : 'info'" size="small">{{ cap.multimodal ? '支持' : '不支持' }}</el-tag>
            </el-descriptions-item>
            <el-descriptions-item label="Mock 模式">
              <el-tag :type="cap.is_mock ? 'warning' : 'info'" size="small">{{ cap.is_mock ? '是' : '否' }}</el-tag>
            </el-descriptions-item>
          </el-descriptions>
        </el-card>
      </el-col>

      <el-col :span="16">
        <el-card shadow="never">
          <template #header>模型目录</template>
          <el-table :data="models" v-loading="loading" stripe size="small">
            <el-table-column prop="name" label="逻辑名" min-width="150" />
            <el-table-column prop="provider" label="厂商" width="120" />
            <el-table-column prop="max_tokens" label="Max Tokens" width="110" />
            <el-table-column label="ToolCall" width="90">
              <template #default="{ row }">
                <el-tag :type="row.supports_tool_call ? 'success' : 'info'" size="small">{{ row.supports_tool_call ? '✓' : '✗' }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="Reasoning" width="90">
              <template #default="{ row }">
                <el-tag :type="row.supports_reasoning ? 'success' : 'info'" size="small">{{ row.supports_reasoning ? '✓' : '✗' }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="状态" width="90">
              <template #default="{ row }">
                <el-tag :type="row.status === 'enabled' ? 'success' : 'info'" size="small">{{ row.status }}</el-tag>
              </template>
            </el-table-column>
          </el-table>
        </el-card>
      </el-col>
    </el-row>

    <el-card shadow="never" style="margin-top: 14px">
      <template #header>用量统计（总 tokens：{{ usage.total_tokens }}，总成本：¥{{ usage.total_cost?.toFixed?.(4) ?? '0' }}）</template>
      <el-table :data="usage.records" v-loading="usageLoading" stripe size="small">
        <el-table-column prop="created_at" label="时间" width="180">
          <template #default="{ row }">{{ fmtTime(row.created_at) }}</template>
        </el-table-column>
        <el-table-column prop="model" label="模型" width="140" />
        <el-table-column prop="prompt_tokens" label="输入" width="90" />
        <el-table-column prop="completion_tokens" label="输出" width="90" />
        <el-table-column label="成本" width="90">
          <template #default="{ row }">¥{{ row.cost?.toFixed?.(4) ?? '0' }}</template>
        </el-table-column>
        <el-table-column prop="degraded" label="降级" width="80">
          <template #default="{ row }">
            <el-tag v-if="row.degraded" type="warning" size="small">是</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="request_id" label="RequestID" min-width="200" show-overflow-tooltip />
      </el-table>
      <el-pagination
        v-model:current-page="usagePage" :total="usage.meta?.total || 0" :page-size="20"
        layout="total, prev, pager, next" class="pager" @current-change="loadUsage"
      />
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { llmApi } from '../../api'
import type { Capabilities, ModelInfo, UsageData } from '../../types'

const loading = ref(false)
const usageLoading = ref(false)
const models = ref<ModelInfo[]>([])
const cap = reactive<Capabilities>({ tool_call: false, reasoning: false, multimodal: false, embeddings: false, provider: '', is_mock: false })
const usage = reactive<UsageData>({ records: [], meta: { page: 1, page_size: 20, total: 0 }, total_tokens: 0, total_cost: 0 })
const usagePage = ref(1)

function fmtTime(ts: number) {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString('zh-CN')
}

async function loadUsage() {
  usageLoading.value = true
  try {
    const d = await llmApi.usage({ page: usagePage.value, page_size: 20 })
    Object.assign(usage, d)
  } finally {
    usageLoading.value = false
  }
}

onMounted(async () => {
  loading.value = true
  try {
    const [m, c] = await Promise.all([llmApi.models(), llmApi.capabilities()])
    models.value = m.models
    Object.assign(cap, c)
  } finally {
    loading.value = false
  }
  loadUsage()
})
</script>

<style scoped>
.pager { margin-top: 12px; justify-content: flex-end; }
</style>
