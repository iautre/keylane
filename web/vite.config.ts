import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  build: { outDir: '../internal/webui/dist', emptyOutDir: true },
  server: { proxy: { '/admin/api': 'http://localhost:8080', '/v1': 'http://localhost:8080', '/health': 'http://localhost:8080', '/ready': 'http://localhost:8080' } },
})
