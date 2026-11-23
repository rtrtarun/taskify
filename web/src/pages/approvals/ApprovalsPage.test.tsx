import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, vi } from "vitest";
import type { AgentJob } from "../../api/agents";
import type { TaskDetail } from "../../api/tasks";
import { SessionStore } from "../../auth/session";
import { json, meHandler, mockApi, renderWithProviders, signInAs, type Call } from "../../test-utils";
import ApprovalsPage from "./ApprovalsPage";

afterEach(() => vi.unstubAllGlobals());

const TASK = "0193a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b";
const JOB = "0193a1b2-c3d4-7e5f-8a6b-aaaaaaaaaaaa";

function task(over: Partial<TaskDetail> = {}): TaskDetail {
  return {
    id: TASK, tenant_id: "t", workspace_id: "ws_core", title: "Fix checkout", description: "It crashes", status: "todo", priority: "p3_medium",
    creator_id: "usr_bob", assignee_id: "usr_bob", labels: ["bug"], metadata: {}, watcher_ids: [], due_at: null, remind_at: null,
    completed_at: null, archived_at: null, blocked_reason: null, version: 2, created_at: "2026-11-20T10:00:00Z", updated_at: "2026-11-20T10:00:00Z",
    comments: [], ...over,
  } as TaskDetail;
}

function job(over: Partial<AgentJob> = {}): AgentJob {
  return {
    id: JOB, task_id: TASK, agent_type: "triage", status: "awaiting_approval", base_task_version: 2,
    proposed_actions: [{ tool: "UpdateTaskAttributes", arguments: { task_id: TASK, priority: "p1_critical", labels: ["bug", "payments"], reasoning: "Payments are down." }, call_id: "c1" }],
    execution_result: null, error_message: null, attempts: 1, decided_by: null, decided_at: null,
    created_at: "2026-11-20T10:00:00Z", updated_at: "2026-11-20T10:05:00Z", ...over,
  } as AgentJob;
}

const inbox = (...jobs: AgentJob[]) => json(200, { agent_jobs: jobs, next_cursor: null }, { ETag: 'W/"x"' });
const isInbox = (c: Call) => c.method === "GET" && c.path === "/api/v1/agent-jobs";
const isTask = (c: Call) => c.method === "GET" && c.path === `/api/v1/tasks/${TASK}`;
const decision = (c: Call, what: "apply" | "dismiss") => c.method === "POST" && c.path === `/api/v1/agent-jobs/${JOB}/${what}`;

async function setup(...handlers: Parameters<typeof mockApi>) {
  const api = mockApi(meHandler("member"), ...handlers);
  const store = new SessionStore();
  await signInAs(store);
  const utils = renderWithProviders(<ApprovalsPage pollMs={60_000} />, { store, route: "/approvals" });
  return { api, ...utils };
}

describe("approvals inbox states", () => {
  test("loading, then nothing waiting", async () => {
    await setup((c) => (isInbox(c) ? inbox() : undefined));
    expect(screen.getByRole("status")).toHaveTextContent("Loading proposals");
    expect(await screen.findByText("Nothing is waiting for your decision.")).toBeInTheDocument();
  });

  test("an error offers a retry", async () => {
    let fail = true;
    await setup((c) => (isInbox(c) ? (fail ? json(500, { error: { code: "internal_error", message: "internal error" } }) : inbox()) : undefined));
    expect(await screen.findByRole("alert", undefined, { timeout: 4000 })).toHaveTextContent("internal error");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("Nothing is waiting for your decision.")).toBeInTheDocument();
  });

  test("a failed refresh keeps the last list", async () => {
    let n = 0;
    const { client } = await setup((c) => (isInbox(c) ? (n++ === 0 ? inbox(job()) : json(500, { error: { code: "internal_error", message: "x" } })) : isTask(c) ? json(200, task()) : undefined));
    await screen.findByRole("button", { name: "Approve" });
    await client.refetchQueries({ queryKey: ["agent-jobs", "awaiting"] });
    expect(await screen.findByText(/Could not refresh/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Approve" })).toBeInTheDocument();
  });
});

describe("a proposal", () => {
  test("shows the agent, current and proposed values side by side, and its reasoning", async () => {
    await setup((c) => (isInbox(c) ? inbox(job()) : isTask(c) ? json(200, task()) : undefined));
    const card = await screen.findByRole("article", { name: "Proposal from Triage agent" });
    expect(await within(card).findByRole("link", { name: "Fix checkout" })).toHaveAttribute("href", `/tasks/${TASK}`);
    const table = within(card).getByRole("table");
    await waitFor(() => expect(within(table).getByRole("row", { name: /Priority/ })).toHaveTextContent("P3 Medium"));
    const priority = within(table).getByRole("row", { name: /Priority/ });
    expect(within(priority).getAllByRole("cell").map((c) => c.textContent)).toEqual(["P3 Medium", "P1 Critical"]);
    const labels = within(table).getByRole("row", { name: /Labels/ });
    expect(within(labels).getAllByRole("cell").map((c) => c.textContent)).toEqual(["bug", "bug, payments"]);
    expect(within(table).queryByText("task_id")).toBeNull();
    expect(within(card).getByLabelText("Reasoning")).toHaveTextContent("Payments are down.");
    expect(screen.queryByText(/out of date/)).toBeNull();
  });

  test("other tools line up against what the task has now", async () => {
    const proposals = [
      { tool: "ReassignTask", arguments: { task_id: TASK, assignee_id: "usr_carol", reasoning: "r" } },
      { tool: "ShiftDueDate", arguments: { task_id: TASK, due_at: "2026-12-01T09:00:00Z", reasoning: "r" } },
      { tool: "ArchiveTask", arguments: { task_id: TASK, reasoning: "r" } },
      { tool: "DraftResolutionComment", arguments: { task_id: TASK, body: "Try restarting", reasoning: "r" } },
    ];
    await setup((c) => (isInbox(c) ? inbox(job({ agent_type: "blocker_investigator", proposed_actions: proposals as AgentJob["proposed_actions"] })) : isTask(c) ? json(200, task()) : undefined));
    const card = await screen.findByRole("article", { name: "Proposal from Blocker investigator" });
    await within(card).findByText("usr_bob");
    expect(within(within(card).getByRole("region", { name: "Reassign the task" })).getAllByRole("cell").map((c) => c.textContent)).toEqual(["usr_bob", "usr_carol"]);
    expect(within(within(card).getByRole("region", { name: "Move the due date" })).getAllByRole("cell").map((c) => c.textContent)).toEqual(["None", "2026-12-01 09:00Z"]);
    expect(within(within(card).getByRole("region", { name: "Archive the task" })).getAllByRole("cell").map((c) => c.textContent)).toEqual(["To do", "Archived"]);
    expect(within(within(card).getByRole("region", { name: "Post a comment" })).getAllByRole("cell").map((c) => c.textContent)).toEqual(["-", "Try restarting"]);
  });

  test("text from an agent is never markup", async () => {
    const evil = '<img src=x onerror="alert(1)">';
    const { container } = await setup((c) =>
      isInbox(c) ? inbox(job({ proposed_actions: [{ tool: "UpdateTaskAttributes", arguments: { task_id: TASK, description: evil, reasoning: "<script>x</script>" } }] as AgentJob["proposed_actions"] })) : isTask(c) ? json(200, task()) : undefined,
    );
    await screen.findByText(evil);
    expect(container.querySelector("img, script")).toBeNull();
  });

  test("a stale proposal is flagged before the person decides", async () => {
    await setup((c) => (isInbox(c) ? inbox(job({ base_task_version: 1 })) : isTask(c) ? json(200, task({ version: 3 })) : undefined));
    expect(await screen.findByText(/out of date: the task has changed since the agent looked at it \(it was version 1, it is now version 3\)/)).toBeInTheDocument();
  });
});

describe("deciding", () => {
  test("approve sends the job's apply and says it was applied", async () => {
    let decided = false;
    const { api } = await setup(
      (c) => (isInbox(c) ? inbox(...(decided ? [] : [job()])) : isTask(c) ? json(200, task()) : undefined),
      (c) => {
        if (!decision(c, "apply")) return undefined;
        decided = true;
        return json(200, { job: job({ status: "completed" }), changed: true });
      },
    );
    await userEvent.click(await screen.findByRole("button", { name: "Approve" }));
    expect(await screen.findByText(/Approved and applied: Triage agent's proposal for "Fix checkout"/)).toBeInTheDocument();
    expect(await screen.findByText("Nothing is waiting for your decision.")).toBeInTheDocument();
    expect(api.calls.filter((c) => decision(c, "apply"))).toHaveLength(1);
    expect(api.calls.some((c) => decision(c, "dismiss"))).toBe(false);
  });

  test("reject sends dismiss", async () => {
    let decided = false;
    const { api } = await setup(
      (c) => (isInbox(c) ? inbox(...(decided ? [] : [job()])) : isTask(c) ? json(200, task()) : undefined),
      (c) => {
        if (!decision(c, "dismiss")) return undefined;
        decided = true;
        return json(200, { job: job({ status: "dismissed" }), changed: true });
      },
    );
    await userEvent.click(await screen.findByRole("button", { name: "Reject" }));
    expect(await screen.findByText(/Rejected: Triage agent's proposal for "Fix checkout"/)).toBeInTheDocument();
    expect(api.calls.some((c) => decision(c, "apply"))).toBe(false);
  });

  test("a stale proposal the server refuses shows a clear message", async () => {
    let decided = false;
    await setup(
      (c) => (isInbox(c) ? inbox(...(decided ? [] : [job()])) : isTask(c) ? json(200, task()) : undefined),
      (c) => {
        if (!decision(c, "apply")) return undefined;
        decided = true;
        return json(409, { error: { code: "stale_version", message: "task moved on" } });
      },
    );
    await userEvent.click(await screen.findByRole("button", { name: "Approve" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("This proposal is out of date: the task changed after the agent made it, so nothing was applied");
    expect(alert).not.toHaveTextContent("task moved on");
    await waitFor(() => expect(screen.queryByRole("button", { name: "Approve" })).toBeNull());
  });

  test("someone else decided first, and a person who may not decide, are told so", async () => {
    let code = "already_decided";
    let status = 409;
    await setup(
      (c) => (isInbox(c) ? inbox(job()) : isTask(c) ? json(200, task()) : undefined),
      (c) => (decision(c, "apply") ? json(status, { error: { code, message: "m" } }) : undefined),
    );
    await userEvent.click(await screen.findByRole("button", { name: "Approve" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Someone else already decided this proposal");
    code = "forbidden";
    status = 403;
    await userEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Only the task's assignee, its creator, or an admin can decide this proposal."));
  });
});
