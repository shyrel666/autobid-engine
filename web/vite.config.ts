import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// 开发模式把 /api 与 /healthz 代理到本地 AutoBid 服务（AUTOBID_HTTP_ADDR，默认 127.0.0.1:8080）。
const backend = process.env.AUTOBID_BACKEND ?? 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: {
      '/api': { target: backend, changeOrigin: true },
      '/healthz': { target: backend, changeOrigin: true },
    },
  },
  build: { outDir: 'dist', emptyOutDir: true },
})
