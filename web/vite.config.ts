import { fileURLToPath, URL } from 'node:url';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import { loadEnv } from 'vite';
import { defineConfig } from 'vitest/config';

export default defineConfig(({ mode }) => {
  const localEnv = loadEnv(mode, fileURLToPath(new URL('.', import.meta.url)), 'VITE_');
  const apiProxyTarget = process.env.VITE_API_PROXY_TARGET ?? localEnv.VITE_API_PROXY_TARGET ?? 'http://127.0.0.1:8080';
  const allowedHosts = (process.env.VITE_DEV_ALLOWED_HOSTS ?? localEnv.VITE_DEV_ALLOWED_HOSTS ?? '')
    .split(',').map((host) => host.trim()).filter(Boolean);

  return {
    plugins: [react(), tailwindcss()],
    resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
    server: {
      port: 5173,
      allowedHosts,
      proxy: {
        // Cookie-session Origin checks must see the browser's console Host.
        '/api': { target: apiProxyTarget, changeOrigin: false },
        '/healthz': { target: apiProxyTarget, changeOrigin: false },
      },
    },
    build: { sourcemap: false, chunkSizeWarningLimit: 650, rollupOptions: { output: { manualChunks(id) { if (id.includes('recharts') || id.includes('d3-')) return 'charts'; if (id.includes('react-virtuoso')) return 'virtualization'; if (id.includes('@base-ui') || id.includes('@floating-ui')) return 'ui'; } } } },
    test: { environment: 'jsdom', setupFiles: './src/test/setup.ts', include: ['src/**/*.test.{ts,tsx}'], css: false },
  };
});
