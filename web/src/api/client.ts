// HTTP 客户端：token 注入 + 统一错误处理 + 401 跳登录
import axios from 'axios'
import { ElMessage } from 'element-plus'

const TOKEN_KEY = 'aegis_token'
const USER_KEY = 'aegis_user'

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) || ''
}

export function setAuth(token: string, user: unknown) {
  localStorage.setItem(TOKEN_KEY, token)
  localStorage.setItem(USER_KEY, JSON.stringify(user))
}

export function getStoredUser<T>(): T | null {
  const raw = localStorage.getItem(USER_KEY)
  if (!raw) return null
  try {
    return JSON.parse(raw) as T
  } catch {
    return null
  }
}

export function clearAuth() {
  localStorage.removeItem(TOKEN_KEY)
  localStorage.removeItem(USER_KEY)
}

// axios 实例：baseURL 空（开发走 vite 代理 / 生产同源）
const http = axios.create({
  timeout: 60000,
  headers: { 'Content-Type': 'application/json' },
})

http.interceptors.request.use((config) => {
  const token = getToken()
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

http.interceptors.response.use(
  (resp) => resp,
  (err) => {
    const status = err.response?.status
    if (status === 401) {
      clearAuth()
      if (window.location.pathname !== '/login') {
        window.location.href = '#/login'
      }
    } else {
      const msg = err.response?.data?.error?.message || err.message || '请求失败'
      ElMessage.error(msg)
    }
    return Promise.reject(err)
  },
)

export function isAdmin(): boolean {
  const user = getStoredUser<{ roles: string[] }>()
  return !!user?.roles?.includes('admin')
}

export default http
