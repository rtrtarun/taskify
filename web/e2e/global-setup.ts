import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { API_PORT, JWT_SECRET, MOCK_PORT, WEB_PORT } from "./ports";

// Starts what the browser tests need, from this checkout: a throwaway
// migrated database, the mock AgentPulse, and the Taskify server. Everything
// is removed again in the returned teardown. The database comes from
// E2E_DATABASE_URL (a role with CREATEDB); the default is the compose
// Postgres on localhost:5433.
const root = path.resolve(import.meta.dirname, "../..");
const exe = process.platform === "win32" ? ".exe" : "";

async function waitFor(url: string, what: string, children: ChildProcess[]): Promise<void> {
  const deadline = Date.now() + 60_000;
  while (Date.now() < deadline) {
    if (children.some((c) => c.exitCode !== null)) throw new Error(`${what} exited while starting`);
    try {
      if ((await fetch(url)).ok) return;
    } catch {
      /* not up yet */
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`${what} did not start at ${url}`);
}

export default async function globalSetup(): Promise<() => Promise<void>> {
  const base = process.env.E2E_DATABASE_URL ?? "postgres://taskify:taskify@localhost:5433/taskify?sslmode=disable";
  const run = (args: string[]) => execFileSync("go", args, { cwd: root, encoding: "utf8", stdio: ["ignore", "pipe", "inherit"] }).trim();

  const dbURL = run(["run", "./tools/e2edb", "create", base]);
  const dir = mkdtempSync(path.join(tmpdir(), "taskify-e2e-"));
  const children: ChildProcess[] = [];

  const teardown = async () => {
    for (const c of children) c.kill();
    await new Promise((r) => setTimeout(r, 300));
    try {
      run(["run", "./tools/e2edb", "drop", dbURL]);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  };

  try {
    run(["build", "-o", path.join(dir, `taskify${exe}`), "./cmd/taskify"]);
    run(["build", "-o", path.join(dir, `mock-agentpulse${exe}`), "./cmd/mock-agentpulse"]);

    const mock = spawn(path.join(dir, `mock-agentpulse${exe}`), [], { env: { ...process.env, PORT: String(MOCK_PORT) }, stdio: "ignore" });
    children.push(mock);
    const server = spawn(path.join(dir, `taskify${exe}`), [], {
      stdio: "ignore",
      env: {
        ...process.env,
        ENV: "production",
        PORT: String(API_PORT),
        DATABASE_URL: dbURL,
        PUBLIC_BASE_URL: `http://127.0.0.1:${API_PORT}`,
        APP_BASE_URL: `http://127.0.0.1:${WEB_PORT}`,
        AGENTPULSE_BASE_URL: `http://127.0.0.1:${MOCK_PORT}`,
        AGENTPULSE_API_KEY: "e2e-key",
        AGENTPULSE_WEBHOOK_SECRET: "e2e-webhook-secret",
        JWT_SECRET,
        RATE_LIMIT_RPS: "1000",
        RATE_LIMIT_BURST: "1000",
        OUTBOX_POLL_INTERVAL: "50ms",
        OUTBOX_RETRY_MAX_DELAY: "200ms",
      },
    });
    children.push(server);
    await waitFor(`http://127.0.0.1:${API_PORT}/healthz`, "the Taskify server", children);
    await waitFor(`http://127.0.0.1:${MOCK_PORT}/healthz`, "the mock AgentPulse", children);
  } catch (e) {
    await teardown();
    throw e;
  }
  return teardown;
}
