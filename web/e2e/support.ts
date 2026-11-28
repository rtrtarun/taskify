import { createHmac, randomBytes } from "node:crypto";
import { expect, type Page } from "@playwright/test";
import { API_URL, JWT_SECRET, LLM_URL, MOCK_URL } from "./ports";

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
  await expect(page.getByRole("navigation", { name: "Main" })).toBeVisible();
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

/** Scripts the mock model: it answers requests that mention `match` (a task ID) with these answers, in order. */
export async function scriptLLM(match: string, answers: object[]): Promise<void> {
  const res = await fetch(`${LLM_URL}/_mock/scripts`, { method: "POST", body: JSON.stringify([{ match, answers }]) });
  if (!res.ok) throw new Error(`script the model: ${res.status}`);
}

/**
 * Makes the tenant's workspace use agents with approval for every change, and
 * only the agent named, so nothing else calls the model. Call it after
 * creating the task a test is about, which would otherwise wake the agent.
 */
export async function enableAgent(t: TestTenant, agent: string): Promise<void> {
  const all = ["triage", "decomposer", "blocker_investigator", "digest", "learning"];
  await call(t.admin, "PATCH", `/api/v1/workspaces/${t.workspace}/settings`, {
    agent_settings: { agents_enabled: true, require_hitl_for_all: true, disabled_agents: all.filter((a) => a !== agent) },
  });
}

export interface ApiJob {
  id: string;
  status: string;
  agent_type: string;
  base_task_version: number | null;
}

/**
 * Has the triage agent propose a priority for a task: the model is scripted
 * to call UpdateTaskAttributes, the task is edited to wake the agent, and the
 * job waits for approval. Returns the job and the task's version at that time.
 */
export async function proposePriority(t: TestTenant, title: string, priority: string, reasoning: string): Promise<{ task: ApiTask; job: ApiJob }> {
  const task = await createTask(t, title); // before agents are on, so creating it wakes none
  await enableAgent(t, "triage");
  await scriptLLM(task.id, [
    { tool_calls: [{ name: "UpdateTaskAttributes", arguments: { task_id: task.id, priority, reasoning } }] },
    { content: "Proposed a priority." },
  ]);
  await call(t.member, "PATCH", `/api/v1/tasks/${task.id}`, { description: "Please look at this" }, { "If-Match": `"${task.version}"` });
  let job: ApiJob | undefined;
  for (let i = 0; i < 100 && !job; i++) {
    const list = await call<{ agent_jobs: ApiJob[] }>(t.member, "GET", `/api/v1/agent-jobs?task_id=${task.id}&status=awaiting_approval`);
    job = list.agent_jobs[0];
    if (!job) await new Promise((r) => setTimeout(r, 100));
  }
  if (!job) throw new Error("the agent never proposed anything");
  return { task, job };
}
