import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

const hub = process.env.CAT_URL || 'http://127.0.0.1:7788'

export default defineConfig({
  plugins: [vue(), tailwindcss()],
  server: { proxy: { '/api': hub, '/ingress': hub } },
  build: { outDir: 'dist', emptyOutDir: true },
})
