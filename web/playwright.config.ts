import { defineConfig, devices } from "@playwright/test";

// The page is served by `vite preview`, which proxies /api to the Taskify
// server named by API_PROXY_TARGET. Tests that need the API expect the stack
// from scripts/e2e-stack.sh to be running (see web/README.md).
const port = Number(process.env.E2E_WEB_PORT ?? 4173);

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: { baseURL: `http://127.0.0.1:${port}`, trace: "retain-on-failure" },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: `npm run build && npx vite preview --host 127.0.0.1 --port ${port} --strictPort`,
    url: `http://127.0.0.1:${port}`,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
  },
});
