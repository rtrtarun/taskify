import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, vi } from "vitest";
import { SessionStore } from "../../auth/session";
import { json, meHandler, mockApi, renderWithProviders, signInAs, type Call } from "../../test-utils";
import AppRoutes from "../../AppRoutes";

afterEach(() => vi.unstubAllGlobals());

const SUGG = {
  id: "sg_1", status: "PENDING_APPROVAL", rule_id: null, event_pattern: { event_type: "task.overdue", label: null },
  current_adjustment: 0, proposed_adjustment: -5, signal_count: 12, rationale: "People dismiss these.",
};
const JOB = "0193a1b2-c3d4-7e5f-8a6b-aaaaaaaaaaaa";
const job = {
  id: JOB, task_id: null, agent_type: "digest", status: "completed", base_task_version: null, proposed_actions: null, execution_result: null,
  error_message: null, attempts: 1, decided_by: null, decided_at: null, created_at: "2026-11-20T10:00:00Z", updated_at: "2026-11-20T10:05:00Z",
};
const WS = { id: "ws_core", name: "Core", agent_settings: { agents_enabled: true, disabled_agents: [], require_hitl_for_all: false, max_subtasks_per_decomposition: 8 }, created_at: "2026-11-20T10:00:00Z", updated_at: "2026-11-20T10:00:00Z" };

async function setup(role: "admin" | "member", route: string, ...handlers: Parameters<typeof mockApi>) {
  const api = mockApi(meHandler(role), ...handlers);
  const store = new SessionStore();
  await signInAs(store, role);
  renderWithProviders(<AppRoutes />, { store, route });
  return api;
}

const is = (method: string, path: string) => (c: Call) => c.method === method && c.path === path;

test("a member who opens an admin URL is sent to the task list", async () => {
  const api = await setup("member", "/admin/learning", (c) => (c.path === "/api/v1/tasks" ? json(200, { tasks: [], next_cursor: null }) : undefined));
  expect(await screen.findByRole("heading", { name: "Tasks" })).toBeInTheDocument();
  expect(api.calls.some((c) => c.path.startsWith("/api/v1/admin"))).toBe(false);
});

test("approve a learning suggestion", async () => {
  let decided = false;
  const api = await setup(
    "admin",
    "/admin",
    (c) => (is("GET", "/api/v1/admin/learning/suggestions")(c) ? json(200, { suggestions: [decided ? { ...SUGG, status: "APPROVED" } : SUGG] }) : undefined),
    (c) => {
      if (!is("POST", "/api/v1/admin/learning/suggestions/sg_1/decision")(c)) return undefined;
      decided = true;
      return json(200, { ...SUGG, status: "APPROVED" });
    },
  );
  const card = await screen.findByRole("article", { name: /Suggestion for Notifications of type task.overdue/ });
  expect(within(card).getByText(/12 feedback signals/)).toBeInTheDocument();
  expect(within(card).getByLabelText("Rationale")).toHaveTextContent("People dismiss these.");
  await userEvent.click(within(card).getByRole("button", { name: "Approve" }));
  expect(await screen.findByText(/Approved: Notifications of type task.overdue/)).toBeInTheDocument();
  expect(api.calls.find((c) => c.method === "POST")?.body).toEqual({ decision: "approve" });
  expect(await screen.findByText("No suggestion is waiting for a decision.")).toBeInTheDocument();
});

test("an already decided suggestion is explained", async () => {
  await setup(
    "admin",
    "/admin/learning",
    (c) => (c.method === "GET" ? json(200, { suggestions: [SUGG] }) : undefined),
    (c) => (c.method === "POST" ? json(409, { error: { code: "conflict", message: "x" } }) : undefined),
  );
  await userEvent.click(await screen.findByRole("button", { name: "Reject" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("already decided");
});

test("dead letters show the last error and can be replayed", async () => {
  const row = { id: "0193a1b2-c3d4-7e5f-8a6b-bbbbbbbbbbbb", aggregate_id: JOB, event_type: "task.created", idempotency_key: "k", status: "dead_letter", attempts: 5, max_attempts: 5, next_attempt_at: "2026-11-20T10:00:00Z", last_error: "502 from AgentPulse", created_at: "2026-11-20T10:00:00Z", dispatched_at: null, payload: {} };
  let replayed = false;
  const api = await setup(
    "admin",
    "/admin/delivery",
    (c) => (is("GET", "/api/v1/admin/outbox")(c) ? json(200, { entries: replayed ? [] : [row], next_cursor: null }) : undefined),
    (c) => {
      if (!is("POST", `/api/v1/admin/outbox/${row.id}/replay`)(c)) return undefined;
      replayed = true;
      return json(200, { ...row, status: "pending" });
    },
  );
  expect(await screen.findByText("502 from AgentPulse")).toBeInTheDocument();
  expect(api.calls.find((c) => c.path === "/api/v1/admin/outbox")?.url.searchParams.get("status")).toBe("dead_letter");
  await userEvent.click(screen.getByRole("button", { name: /Replay task.created/ }));
  expect(await screen.findByText("Queued task.created for delivery again.")).toBeInTheDocument();
  expect(await screen.findByText("No row is in the dead letter queue.")).toBeInTheDocument();
});

test("a job's runs show its turns and token use", async () => {
  await setup(
    "admin",
    "/admin/jobs",
    (c) => (is("GET", "/api/v1/agent-jobs")(c) ? json(200, { agent_jobs: [job], next_cursor: null }) : undefined),
    (c) =>
      is("GET", `/api/v1/admin/agent-jobs/${JOB}/runs`)(c)
        ? json(200, {
            job,
            runs: [
              { turn: 1, model: "m", tool_name: "x", tool_args: null, tool_result: null, outcome: "tool_call", prompt_tokens: 100, completion_tokens: 20, latency_ms: 300, created_at: "2026-11-20T10:00:00Z" },
              { turn: 2, model: "m", tool_name: null, tool_args: null, tool_result: null, outcome: "final", prompt_tokens: 50, completion_tokens: 5, latency_ms: 100, created_at: "2026-11-20T10:00:01Z" },
            ],
          })
        : undefined,
  );
  await userEvent.click(await screen.findByRole("button", { name: /Show runs of Digest agent/ }));
  expect(await screen.findByText(/2 turns, 150 prompt tokens and 25 completion tokens \(175 in all\)/)).toBeInTheDocument();
});

test("workspace settings save, and rules can be bootstrapped", async () => {
  const api = await setup(
    "admin",
    "/admin/workspaces",
    (c) => (is("GET", "/api/v1/workspaces")(c) ? json(200, { workspaces: [WS], next_cursor: null }) : undefined),
    (c) => (is("GET", "/api/v1/workspaces/ws_core/settings")(c) ? json(200, WS) : undefined),
    (c) => (is("PATCH", "/api/v1/workspaces/ws_core/settings")(c) ? json(200, WS) : undefined),
    (c) => (is("POST", "/api/v1/workspaces/ws_core/bootstrap-rules")(c) ? json(200, { rules: [{ key: "a", agentpulse_rule_id: "r1", created_at: "2026-11-20T10:00:00Z" }] }) : undefined),
  );
  await userEvent.click(await screen.findByLabelText("Require approval for every agent change"));
  await userEvent.click(screen.getByLabelText("Learning agent"));
  await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
  expect(await screen.findByText("Settings saved.")).toBeInTheDocument();
  expect(api.calls.find((c) => c.method === "PATCH")?.body).toEqual({
    agent_settings: { agents_enabled: true, require_hitl_for_all: true, max_subtasks_per_decomposition: 8, disabled_agents: ["learning"] },
  });
  await userEvent.click(screen.getByRole("button", { name: "Bootstrap rules" }));
  expect(await screen.findByText("1 rule is registered.")).toBeInTheDocument();
});
