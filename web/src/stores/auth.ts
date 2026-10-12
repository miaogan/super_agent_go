import { defineStore } from 'pinia'
import { authApi } from '../api'
import { getStoredUser, setAuth, clearAuth, getToken } from '../api/client'
import type { LoginUser } from '../types'

export const useAuthStore = defineStore('auth', {
  state: () => ({
    token: getToken(),
    user: getStoredUser<LoginUser>(),
  }),
  getters: {
    isLoggedIn: (s) => !!s.token,
    isAdmin: (s) => !!s.user?.roles?.includes('admin'),
  },
  actions: {
    async login(username: string, password: string) {
      const data = await authApi.login(username, password)
      this.token = data.access_token
      this.user = data.user
      setAuth(data.access_token, data.user)
      return data
    },
    logout() {
      this.token = ''
      this.user = null
      clearAuth()
    },
  },
})
