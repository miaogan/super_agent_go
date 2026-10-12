<template>
  <el-container class="layout">
    <el-aside width="210px" class="aside">
      <div class="logo">Aegis</div>
      <el-menu :default-active="activeMenu" router class="menu">
        <el-menu-item index="/agents">
          <el-icon><ChatDotRound /></el-icon><span>智能体</span>
        </el-menu-item>
        <el-menu-item index="/kbs">
          <el-icon><Collection /></el-icon><span>知识库</span>
        </el-menu-item>
        <el-menu-item index="/system/models">
          <el-icon><Cpu /></el-icon><span>模型信息</span>
        </el-menu-item>
        <el-menu-item v-if="auth.isAdmin" index="/system/users">
          <el-icon><UserFilled /></el-icon><span>用户管理</span>
        </el-menu-item>
      </el-menu>
    </el-aside>

    <el-container>
      <el-header class="header">
        <div class="header-title">{{ title }}</div>
        <el-dropdown @command="onCommand">
          <span class="user-chip">
            <el-avatar :size="28">{{ avatarText }}</el-avatar>
            <span class="uname">{{ auth.user?.nickname || auth.user?.username }}</span>
          </span>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item command="logout">退出登录</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </el-header>

      <el-main class="main">
        <router-view />
      </el-main>
    </el-container>
  </el-container>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ChatDotRound, Collection, Cpu, UserFilled } from '@element-plus/icons-vue'
import { useAuthStore } from '../stores/auth'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const titles: Record<string, string> = {
  agents: '智能体',
  chat: '对话',
  kbs: '知识库',
  'kb-detail': '知识库详情',
  models: '模型信息',
  users: '用户管理',
}
const title = computed(() => titles[String(route.name)] || 'Aegis')
const activeMenu = computed(() => (String(route.name) === 'chat' ? '/agents' : String(route.name) === 'kb-detail' ? '/kbs' : route.path))
const avatarText = computed(() => (auth.user?.nickname || auth.user?.username || 'U').slice(0, 1).toUpperCase())

function onCommand(cmd: string) {
  if (cmd === 'logout') {
    auth.logout()
    router.push('/login')
  }
}
</script>

<style scoped>
.layout { height: 100vh; }
.aside {
  background: #1f2d3d;
  display: flex;
  flex-direction: column;
}
.logo {
  height: 60px;
  line-height: 60px;
  text-align: center;
  color: #fff;
  font-size: 22px;
  font-weight: 600;
  letter-spacing: 2px;
}
.menu {
  border-right: none;
  background: transparent;
  --el-menu-text-color: #c0c4cc;
  --el-menu-hover-bg-color: #2b4a63;
  --el-menu-active-color: #409eff;
}
.header {
  background: #fff;
  border-bottom: 1px solid #e4e7ed;
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.header-title { font-size: 16px; font-weight: 600; color: #303133; }
.user-chip { display: flex; align-items: center; gap: 8px; cursor: pointer; }
.uname { color: #303133; font-size: 14px; }
.main { padding: 16px; overflow: auto; }
</style>
