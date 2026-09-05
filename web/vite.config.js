import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
    plugins: [vue()],
    build: {
        outDir: 'dist',
        chunkSizeWarningLimit: 1500,
    },
    server: {
        port: 3000,
        proxy: {
            '/api': { target: 'http://127.0.0.1:8090', changeOrigin: true },
        },
    },
})
