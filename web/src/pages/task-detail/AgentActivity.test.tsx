import { screen, within } from "@testing-library/react";
import { afterEach, vi } from "vitest";
import type { AgentJobRuns, AgentRun } from "../../api/agents";
import { SessionStore } from "../../auth/session";
import { json, meHandler, mockApi, renderWithProviders, signInAs } from "../../test-utils";
import AgentActivity from "./AgentActivity";

afterEach(() => vi.unstubAllGlobals());

const ID = "0193a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b";

function run(over: Partial<AgentRun> = {}): AgentRun {
  return {
    turn: 1, model: "m", tool_name: null, tool_args: null, tool_result: null, outcome: "final", prompt_tokens: 10, completion_tokens: 5,
    latency_ms: 20, created_at: "2026-11-20T10:00:01Z", ...over,
  } as AgentRun;
}

function entry(agent: string, status: string, runs: AgentRun[], over: Record<string, unknown> = {}): AgentJobRuns {
  return {
    job: {
      id: `job-${agent}-${status}`, task_id: ID, agent_type: agent, status, base_task_version: 1, proposed_actions: null,
      execution_result: null, error_message: null, attempts: 1, decided_by: null, decided_at: null,
      created_at: "2026-11-20T10:00:00Z", updated_at: "2026-11-20T10:00:00Z", ...over,
    },
    runs,
  } as AgentJobRuns;
}

async function setup(runs: AgentJobRuns[]) {
  mockApi(meHandler("member"), (c) => (c.path === `/api/v1/tasks/${ID}/agent-runs` ? json(200, { agent_runs: runs }) : undefined));
  const store = new SessionStore();
  await signInAs(store);
  return renderWithProviders(<AgentActivity taskId={ID} pollMs={60_000} />, { store });
}

describe("agent activity on a task", () => {
  test("no agent has worked on it", async () => {
    await setup([]);
    expect(await screen.findByText("No agent has worked on this task.")).toBeInTheDocument();
  });

  test("names the agent, what changed, and its reasoning, as text", async () => {
    await setup([
      entry("triage", "completed", [
        run({ turn: 1, tool_name: "UpdateTaskAttributes", outcome: "tool_call", tool_args: { task_id: ID, labels: ["bug", "p0"], priority: "p1_critical", reasoning: "Payments are down <b>now</b>." } }),
        run({ turn: 2 }),
      ]),
      entry("blocker_investigator", "awaiting_approval", [
        run({ tool_name: "ReassignTask", outcome: "proposal", tool_args: { task_id: ID, assignee_id: "usr_carol", reasoning: "Carol owns payments." } }),
      ]),
    ]);
    const timeline = await screen.findByRole("list", { name: "Agent activity" });
    const [first, second] = within(timeline).getAllByRole("listitem", { hidden: false }).filter((li) => li.querySelector("h3"));
    expect(within(first).getByRole("heading")).toHaveTextContent("Triage agent (completed)");
    expect(first).toHaveTextContent("Update task details - Done");
    expect(first).toHaveTextContent("Labels: bug, p0");
    expect(first).toHaveTextContent("Priority: P1 Critical");
    expect(first).not.toHaveTextContent("task_id");
    expect(within(first).getByLabelText("Reasoning")).toHaveTextContent("Payments are down <b>now</b>.");
    expect(first.querySelector("b")).toBeNull();

    expect(within(second).getByRole("heading")).toHaveTextContent("Blocker investigator (awaiting approval)");
    expect(second).toHaveTextContent("Reassign the task - Proposed, waiting for a decision");
    expect(second).toHaveTextContent("Assignee: usr_carol");
    expect(within(second).getByRole("link", { name: "Open the approvals inbox" })).toHaveAttribute("href", "/approvals");
  });

  test("a turn that called several tools lists each", async () => {
    await setup([
      entry("decomposer", "completed", [
        run({
          tool_name: "multiple", outcome: "tool_call",
          tool_args: [
            { id: "1", name: "CreateSubtaskDAG", arguments: { parent_task_id: ID, subtasks: [{ title: "A" }, { title: "B", depends_on: ["A"] }], reasoning: "Two steps." } },
            { id: "2", name: "LinkDependencies", arguments: { task_id: ID, depends_on_task_ids: ["x"] } },
          ],
          tool_result: [{ created: 2 }, { linked: 1 }],
        }),
      ]),
    ]);
    const item = (await screen.findAllByRole("heading", { level: 3 }))[0].closest("li")!;
    expect(item).toHaveTextContent("Create subtasks - Done");
    expect(item).toHaveTextContent("Subtasks: A; B (after A)");
    expect(item).toHaveTextContent("Add dependencies - Done");
    expect(within(item).getByLabelText("Reasoning")).toHaveTextContent("Two steps.");
  });

  test("a refused call shows why, and a failed job shows its error", async () => {
    await setup([
      entry("triage", "failed", [run({ tool_name: "ArchiveTask", outcome: "rule_rejected", tool_args: { task_id: ID, reasoning: "Done." }, tool_result: { error: "task is already archived" } })], {
        error_message: "gave up after 3 attempts",
      }),
    ]);
    const item = (await screen.findAllByRole("heading", { level: 3 }))[0].closest("li")!;
    expect(item).toHaveTextContent("Archive the task - Refused by a rule");
    expect(item).toHaveTextContent("task is already archived");
    expect(item).toHaveTextContent("It failed: gave up after 3 attempts");
  });

  test("an error offers a retry", async () => {
    mockApi(meHandler("member"), (c) => (c.path.endsWith("/agent-runs") ? json(500, { error: { code: "internal_error", message: "internal error" } }) : undefined));
    const store = new SessionStore();
    await signInAs(store);
    renderWithProviders(<AgentActivity taskId={ID} pollMs={60_000} />, { store });
    expect(await screen.findByRole("alert", undefined, { timeout: 4000 })).toHaveTextContent("internal error");
    expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });
});
