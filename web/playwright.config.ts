import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  retries: 1,
  use: { baseURL: 'http://127.0.0.1:15174', trace: 'retain-on-failure' },
  webServer: [
    {
      command: 'go run ./cmd/server',
      cwd: '..',
      url: 'http://127.0.0.1:18080/healthz',
      reuseExistingServer: true,
      timeout: 120_000,
      env: { NEWNEXUS_DEMO: 'true', NEWNEXUS_DEV_AUTH: 'true', NEWNEXUS_DEV_MODE: 'true', NEWNEXUS_HTTP_ADDR: '127.0.0.1:18080', NEWNEXUS_GRPC_ADDR: '127.0.0.1:19090' },
    },
    { command: 'npm run dev -- --port 15174', url: 'http://127.0.0.1:15174', reuseExistingServer: true, timeout: 60_000, env: { VITE_API_PROXY_TARGET: 'http://127.0.0.1:18080' } },
  ],
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } } },
    { name: 'tablet', use: { ...devices['Desktop Chrome'], viewport: { width: 820, height: 1180 } } },
    { name: 'mobile', use: { ...devices['Pixel 5'], viewport: { width: 390, height: 844 } } },
    { name: 'reduced-motion', use: { ...devices['Desktop Chrome'], viewport: { width: 1116, height: 987 }, reducedMotion: 'reduce' } },
  ],
});
