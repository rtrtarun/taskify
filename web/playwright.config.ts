import { defineConfig, devices } from "@playwright/test";
import { API_URL, WEB_PORT } from "./e2e/ports";

// The page is served by `vite preview`, which proxies /api to the Taskify
// server that e2e/global-setup.ts starts from this checkout (a throwaway
// database on E2E_DATABASE_URL, default the compose Postgres on :5433, and
// the mock AgentPulse). Nothing is left running or in the database after.
export default defineConfig({
  testDir: "./e2e",
  globalSetup: "./e2e/global-setup.ts",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: { baseURL: `http://127.0.0.1:${WEB_PORT}`, trace: "retain-on-failure" },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: `npm run build && npx vite preview --host 127.0.0.1 --port ${WEB_PORT} --strictPort`,
    env: { API_PROXY_TARGET: API_URL },
    url: `http://127.0.0.1:${WEB_PORT}`,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
  },
});
