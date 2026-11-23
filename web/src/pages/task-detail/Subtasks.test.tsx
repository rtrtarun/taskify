import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, vi } from "vitest";
import type { AgentJob } from "../../api/agents";
import { SessionStore } from "../../auth/session";
import { json, meHandler, mockApi, renderWithProviders, signInAs, type Call } from "../../test-utils";
import Subtasks from "./Subtasks";

afterEach(() => vi.unstubAllGlobals());

const ID = "0193a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b";

function job(status: AgentJob["status"], over: Partial<AgentJob> = {}): AgentJob {
  return {
    id: "0193a1b2-c3d4-7e5f-8a6b-aaaaaaaaaaaa", task_id: ID, agent_type: "decomposer", status, base_task_version: 1,
    proposed_actions: null, execution_result: null, error_message: null, attempts: 0, decided_by: null, decided_at: null,
    created_at: "2026-11-20T10:00:00Z", updated_at: "2026-11-20T10:00:00Z", ...over,
  } as AgentJob;
}

const jobs = (...list: AgentJob[]) => json(200, { agent_jobs: list, next_cursor: null });
const isJobs = (c: Call) => c.method === "GET" && c.path === "/api/v1/agent-jobs";
const isSubtasks = (c: Call) => c.path === `/api/v1/tasks/${ID}/subtasks`;
const isDecompose = (c: Call) => c.method === "POST" && c.path === `/api/v1/tasks/${ID}/decompose`;

async function setup(taskStatus: string, ...handlers: Parameters<typeof mockApi>) {
  const api = mockApi(meHandler("member"), ...handlers);
  const store = new SessionStore();
  await signInAs(store);
  const utils = renderWithProviders(<Subtasks taskId={ID} taskStatus={taskStatus} pollMs={60_000} jobPollMs={30} />, { store });
  return { api, ...utils };
}

describe("Decompose", () => {
  test("shows the job's progress from queued to done, then reads the new subtasks", async () => {
    let polls = 0;
    let created = false;
    let asked = false; // the job exists once the person has clicked
    const sub = { task: { id: "0193a1b2-c3d4-7e5f-8a6b-bbbbbbbbbbbb", title: "Build it", status: "todo" }, depends_on: [] };
    const { api } = await setup(
      "todo",
      (c) => (isSubtasks(c) ? json(200, { subtasks: created ? [sub] : [] }) : undefined),
      (c) => {
        if (isDecompose(c)) {
          asked = true;
          return json(202, job("pending"));
        }
        if (!isJobs(c)) return undefined;
        polls++;
        if (!asked) return jobs();
        if (polls < 8) return jobs(job("running"));
        created = true;
        return jobs(job("completed"));
      },
    );
    expect(await screen.findByText("No subtasks yet.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Decompose" }));
    expect(await screen.findByRole("status", { name: "Decomposition progress" })).toHaveTextContent("Queued");
    await waitFor(() => expect(screen.getByRole("status", { name: "Decomposition progress" })).toHaveTextContent("working on a plan"));
    await waitFor(() => expect(screen.getByRole("status", { name: "Decomposition progress" })).toHaveTextContent("is done"));
    expect(await screen.findByRole("link", { name: "Build it" })).toBeInTheDocument();
    expect(api.calls.filter(isDecompose)).toHaveLength(1);
  });

  test("a plan waiting for approval points to the inbox, and Decompose is off", async () => {
    await setup("todo", (c) => (isSubtasks(c) ? json(200, { subtasks: [] }) : isJobs(c) ? jobs(job("awaiting_approval")) : undefined));
    expect(await screen.findByRole("link", { name: "Review it in the approvals inbox" })).toHaveAttribute("href", "/approvals");
    expect(screen.getByRole("button", { name: "Decompose" })).toBeDisabled();
  });

  test("a failed job says why, and Decompose can be tried again", async () => {
    await setup("todo", (c) => (isSubtasks(c) ? json(200, { subtasks: [] }) : isJobs(c) ? jobs(job("failed", { error_message: "model unreachable" })) : undefined));
    expect(await screen.findByRole("status", { name: "Decomposition progress" })).toHaveTextContent("failed. model unreachable");
    expect(screen.getByRole("button", { name: "Decompose" })).toBeEnabled();
  });

  test("the server's refusal is shown, for a workspace with the decomposer off", async () => {
    await setup("todo", (c) => {
      if (isSubtasks(c)) return json(200, { subtasks: [] });
      if (isJobs(c)) return jobs();
      if (isDecompose(c)) return json(409, { error: { code: "agent_disabled", message: "the decomposer is switched off for this workspace" } });
      return undefined;
    });
    await userEvent.click(await screen.findByRole("button", { name: "Decompose" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("the decomposer is switched off for this workspace");
  });

  test("a finished task cannot be decomposed", async () => {
    await setup("completed", (c) => (isSubtasks(c) ? json(200, { subtasks: [] }) : isJobs(c) ? jobs() : undefined));
    expect(await screen.findByRole("button", { name: "Decompose" })).toBeDisabled();
  });
});

