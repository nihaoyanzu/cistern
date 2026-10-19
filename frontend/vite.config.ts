import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'

const adminPort = process.env.CISTERN_ADMIN_PORT || process.env.MIRRORHUB_ADMIN_PORT || '18082'
const proxyPort = process.env.CISTERN_PROXY_PORT || process.env.MIRRORHUB_PROXY_PORT || '18081'
const adminTarget = `http://127.0.0.1:${adminPort}`
const proxyTarget = `http://127.0.0.1:${proxyPort}`

// 开发期用 Vite 代理替代 Nginx：
// - 管理台 API / metrics → admin（默认 :18082，可由 CISTERN_ADMIN_PORT 覆盖）
// - PyPI 数据面 /simple、/packages/* → proxy（默认 :18081，可由 CISTERN_PROXY_PORT 覆盖）
// - 勿代理裸 /packages，留给 SPA 包检索页
export default defineConfig({
  plugins: [vue(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: Number(process.env.CISTERN_FRONTEND_PORT || process.env.MIRRORHUB_FRONTEND_PORT || 5173),
    proxy: {
      '/api': {
        target: adminTarget,
        changeOrigin: true,
      },
      '/metrics': {
        target: adminTarget,
        changeOrigin: true,
      },
      '/health': {
        target: adminTarget,
        changeOrigin: true,
      },
      '/simple': {
        target: proxyTarget,
        changeOrigin: true,
      },
      // 发行文件走数据面；裸 /packages 留给 SPA 包检索页
      '/packages': {
        target: proxyTarget,
        changeOrigin: true,
        bypass(req) {
          const path = (req.url || '').split('?')[0]
          if (path === '/packages' || path === '/packages/') {
            return path
          }
        },
      },
    },
  },
})
