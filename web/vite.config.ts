import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  // The Go binary embeds this directory via internal/webui.
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    // Keep the embedded payload small; the whole SPA ships inside the binary.
    chunkSizeWarningLimit: 700,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:9977',
        changeOrigin: true,
      },
    },
  },
})
