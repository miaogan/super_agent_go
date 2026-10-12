import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 前端构建产物（dist/）由 gateway 静态托管：
//   compose 挂载 ./web/dist:/app/web + AEGIS_GATEWAY__STATIC_DIR=/app/web
// 开发期走 Vite dev server，API 统一代理到 gateway 8000。
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://127.0.0.1:8000', changeOrigin: true },
      '/healthz': { target: 'http://127.0.0.1:8000', changeOrigin: true },
    },
  },
  build: {
    outDir: 'dist',
    chunkSizeWarningLimit: 1500,
  },
})
