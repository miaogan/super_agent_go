<template>
  <el-card shadow="never">
    <div class="toolbar">
      <el-input
        v-model="keyword" placeholder="搜索用户名/昵称/邮箱" clearable style="width: 240px"
        @keyup.enter="load" @clear="load"
      />
      <el-button type="primary" @click="openCreate">新建用户</el-button>
    </div>

    <el-table :data="list" v-loading="loading" stripe>
      <el-table-column prop="uid" label="UID" width="80" />
      <el-table-column prop="username" label="用户名" min-width="120" />
      <el-table-column prop="nickname" label="昵称" min-width="120" />
      <el-table-column prop="email" label="邮箱" min-width="160" />
      <el-table-column label="角色" width="160">
        <template #default="{ row }">
          <el-tag v-for="r in row.roles" :key="r" size="small" :type="r === 'admin' ? 'danger' : 'info'" style="margin-right: 4px">{{ r }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="90">
        <template #default="{ row }">
          <el-tag :type="row.status === 1 ? 'success' : 'danger'" size="small">{{ row.status === 1 ? '正常' : '禁用' }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="tenant" label="租户" width="100" />
      <el-table-column label="操作" width="180" fixed="right">
        <template #default="{ row }">
          <el-button link @click="openEdit(row)">编辑</el-button>
          <el-popconfirm title="确认删除该用户？" @confirm="remove(row)">
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

    <el-dialog v-model="dlg" :title="editing ? '编辑用户' : '新建用户'" width="480px">
      <el-form :model="form" label-width="90px">
        <el-form-item label="用户名" required>
          <el-input v-model="form.username" :disabled="!!editing" />
        </el-form-item>
        <el-form-item v-if="!editing" label="密码" required>
          <el-input v-model="form.password" type="password" show-password />
        </el-form-item>
        <el-form-item label="昵称">
          <el-input v-model="form.nickname" />
        </el-form-item>
        <el-form-item label="邮箱">
          <el-input v-model="form.email" />
        </el-form-item>
        <el-form-item label="角色">
          <el-checkbox-group v-model="form.roles">
            <el-checkbox value="admin">admin</el-checkbox>
            <el-checkbox value="user">user</el-checkbox>
          </el-checkbox-group>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dlg = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { userApi } from '../../api'
import type { User } from '../../types'

const list = ref<User[]>([])
const loading = ref(false)
const keyword = ref('')
const page = ref(1)
const pageSize = 10
const total = ref(0)

const dlg = ref(false)
const editing = ref<User | null>(null)
const saving = ref(false)
const form = reactive({ username: '', password: '', nickname: '', email: '', roles: ['user'] as string[] })

async function load() {
  loading.value = true
  try {
    const d = await userApi.list({ page: page.value, page_size: pageSize, keyword: keyword.value || undefined })
    list.value = d.users
    total.value = d.meta?.total || 0
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = null
  Object.assign(form, { username: '', password: '', nickname: '', email: '', roles: ['user'] })
  dlg.value = true
}

function openEdit(row: User) {
  editing.value = row
  Object.assign(form, { username: row.username, password: '', nickname: row.nickname || '', email: row.email || '', roles: row.roles || ['user'] })
  dlg.value = true
}

async function save() {
  if (!form.username || (!editing.value && !form.password)) {
    ElMessage.warning('用户名与密码必填')
    return
  }
  saving.value = true
  try {
    if (editing.value) {
      await userApi.update(editing.value.uid, { nickname: form.nickname, email: form.email, roles: form.roles })
    } else {
      await userApi.create({ username: form.username, password: form.password, nickname: form.nickname, email: form.email, roles: form.roles })
    }
    ElMessage.success('保存成功')
    dlg.value = false
    load()
  } finally {
    saving.value = false
  }
}

async function remove(row: User) {
  await userApi.remove(row.uid)
  ElMessage.success('已删除')
  load()
}

onMounted(load)
</script>

<style scoped>
.toolbar { display: flex; justify-content: space-between; margin-bottom: 14px; }
.pager { margin-top: 14px; justify-content: flex-end; }
</style>
