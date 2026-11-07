import { defineConfig } from '@playwright/test';
const external = process.env.NEWNEXUS_E2E_BASE_URL;
export default defineConfig({
  testDir: './e2e', fullyParallel: false, workers: 1, timeout: 45_000,
  globalTeardown: './scripts/e2e-teardown.ts',
  expect: { timeout: 10_000 }, retries: process.env.CI ? 1 : 0,
  reporter: [['list'], ['html', { open: 'never' }]],
  use: { baseURL: external ?? 'http://127.0.0.1:18080', viewport: { width: 1440, height: 1000 }, trace: 'retain-on-failure', screenshot: 'only-on-failure', launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {} },
  projects: [
    { name: 'setup', testMatch: /bootstrap\.setup\.ts/ },
    { name: 'chromium', testMatch: /.*\.spec\.ts/, dependencies: ['setup'], use: { storageState: '../.cache/e2e-state.json' } },
  ],
  webServer: external ? undefined : { command: 'node scripts/e2e-server.mjs', url: 'http://127.0.0.1:18080/healthz', timeout: 60_000, reuseExistingServer: false },
});
