import { createHmac, randomBytes } from "node:crypto";
import { expect, type Page } from "@playwright/test";
import { API_URL, JWT_SECRET, MOCK_URL } from "./ports";

const b64 = (o: object) => Buffer.from(JSON.stringify(o)).toString("base64url");

/** A development token, signed as cmd/devtoken signs it. */
export function token(o: { tenant: string; user: string; role?: "admin" | "member"; ttlSeconds?: number }): string {
  const head = b64({ alg: "HS256", typ: "JWT" });
  const body = b64({
    sub: o.user,
    tenant_id: o.tenant,
    role: o.role ?? "member",
    exp: Math.floor(Date.now() / 1000) + (o.ttlSeconds ?? 3600),
  });
  const sig = createHmac("sha256", JWT_SECRET).update(`${head}.${body}`).digest("base64url");
  return `${head}.${body}.${sig}`;
}

/** Calls the Taskify API directly, to set a test up. */
export async function call<T = unknown>(tok: string, method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
  const res = await fetch(API_URL + path, {
    method,
    headers: { Authorization: `Bearer ${tok}`, "Content-Type": "application/json", ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${text}`);
  return (text ? JSON.parse(text) : undefined) as T;
}

export interface TestTenant {
  tenant: string;
  workspace: string;
  admin: string;
  member: string;
  memberId: string;
}

/** A tenant of its own with one workspace, so tests never share rows. */
export async function newTenant(): Promise<TestTenant> {
  const tenant = `tenant_e2e_${randomBytes(5).toString("hex")}`;
  const admin = token({ tenant, user: "usr_admin", role: "admin" });
  await call(admin, "POST", "/api/v1/workspaces", { id: "ws_e2e", name: "E2E" });
  return { tenant, workspace: "ws_e2e", admin, member: token({ tenant, user: "usr_bob" }), memberId: "usr_bob" };
}

export interface ApiTask {
  id: string;
  version: number;
  title: string;
  priority: string;
}

export function createTask(t: TestTenant, title: string, extra: Record<string, unknown> = {}): Promise<ApiTask> {
  return call<ApiTask>(t.member, "POST", "/api/v1/tasks", { workspace_id: t.workspace, title, assignee_id: t.memberId, ...extra });
}

/** Signs a page in the way a person does: pastes a token. */
export async function signIn(page: Page, tok: string, path = "/tasks"): Promise<void> {
  await page.goto(path);
  await page.getByLabel("Development token").fill(tok);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("link", { name: "Tasks" })).toBeVisible();
}

// The mock AgentPulse's control endpoints (cmd/mock-agentpulse).
export const mock = {
  async seedFeed(tenant: string, user: string, items: object[]): Promise<void> {
    const res = await fetch(`${MOCK_URL}/_mock/feed`, { method: "POST", body: JSON.stringify({ tenant_id: tenant, user_id: user, items }) });
    if (!res.ok) throw new Error(`seed feed: ${res.status}`);
  },
  async fail(method: string, path: string, status: number, count: number): Promise<void> {
    const res = await fetch(`${MOCK_URL}/_mock/fail`, { method: "POST", body: JSON.stringify({ method, path, status, count }) });
    if (!res.ok) throw new Error(`script failure: ${res.status}`);
  },
  async requests(): Promise<{ method: string; path: string; body?: Record<string, unknown> }[]> {
    const res = await fetch(`${MOCK_URL}/_mock/requests`);
    return ((await res.json()) as { requests: { method: string; path: string; body?: Record<string, unknown> }[] }).requests;
  },
  async reset(): Promise<void> {
    await fetch(`${MOCK_URL}/_mock/reset`, { method: "POST" });
  },
};
