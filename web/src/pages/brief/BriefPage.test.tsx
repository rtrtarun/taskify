import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, vi } from "vitest";
import type { MorningBrief } from "../../api/agents";
import { SessionStore } from "../../auth/session";
import { json, meHandler, mockApi, renderWithProviders, signInAs } from "../../test-utils";
import BriefPage from "./BriefPage";

afterEach(() => vi.unstubAllGlobals());

const T1 = "0193a1b2-c3d4-7e5f-8a6b-000000000001";
const T2 = "0193a1b2-c3d4-7e5f-8a6b-000000000002";

function brief(over: Partial<MorningBrief> = {}): MorningBrief {
  return {
    job_id: "0193a1b2-c3d4-7e5f-8a6b-aaaaaaaaaaaa", written_at: "2026-11-23T08:00:05Z", brief_date: "2026-11-23", user_id: "usr_bob",
    top_items: [
      { task_id: T1, title: "Fix checkout", workspace_id: "ws_core", status: "in_progress", priority: "p1_critical", due_at: "2026-11-22T09:00:00Z", overdue: true },
      { task_id: T2, title: "Write docs", workspace_id: "ws_core", status: "todo", priority: "p3_medium", due_at: null, overdue: false },
    ],
    open_tasks: 7,
    pending_approvals: [{ job_id: "j1", agent_type: "triage", task_id: T1, task_title: "Fix checkout", proposed_tools: ["UpdateTaskAttributes"], asked_at: "2026-11-22T12:00:00Z" }],
    pending_approvals_total: 3,
    unblocked_tasks: [{ task_id: T2, title: "Write docs", workspace_id: "ws_core", status: "todo", priority: "p3_medium", due_at: null, overdue: false }],
    unblocked_total: 1,
    text: "Good morning. Start with checkout.\nThen docs.",
    ...over,
  } as MorningBrief;
}

async function setup(handler: Parameters<typeof mockApi>[0]) {
  mockApi(meHandler("member"), handler);
  const store = new SessionStore();
  await signInAs(store);
  return renderWithProviders(<BriefPage />, { store, route: "/brief" });
}

describe("morning brief", () => {
  test("shows the caller's brief: text, top items, approvals, and what was unblocked", async () => {
    await setup((c) => (c.path === "/api/v1/me/brief" ? json(200, brief()) : undefined));
    expect(screen.getByRole("status")).toHaveTextContent("Loading your brief");
    expect(await screen.findByText(/Good morning. Start with checkout./)).toBeInTheDocument();
    expect(screen.getByText(/You have 7 open tasks/)).toBeInTheDocument();

    const top = screen.getByRole("list", { name: "Top items" });
    const first = within(top).getAllByRole("listitem")[0];
    expect(within(first).getByRole("link", { name: "Fix checkout" })).toHaveAttribute("href", `/tasks/${T1}`);
    expect(first).toHaveTextContent("In progress, P1 Critical, due 2026-11-22 09:00Z (overdue)");

    const approvals = screen.getByRole("list", { name: "Pending approvals" });
    expect(approvals).toHaveTextContent("Triage agent proposes Update task details on Fix checkout");
    expect(screen.getByText("and 2 more")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open the approvals inbox" })).toHaveAttribute("href", "/approvals");

    expect(within(screen.getByRole("list", { name: "Unblocked tasks" })).getByRole("link", { name: "Write docs" })).toBeInTheDocument();
  });

  test("an empty brief says what is empty", async () => {
    await setup((c) => (c.path === "/api/v1/me/brief" ? json(200, brief({ top_items: [], pending_approvals: [], pending_approvals_total: 0, unblocked_tasks: [], unblocked_total: 0, open_tasks: 1, text: undefined })) : undefined));
    expect(await screen.findByText("Nothing stands out.")).toBeInTheDocument();
    expect(screen.getByText("Nothing is waiting for you.")).toBeInTheDocument();
    expect(screen.getByText("Nothing was unblocked.")).toBeInTheDocument();
    expect(screen.getByText(/You have 1 open task\./)).toBeInTheDocument();
  });

  test("no brief yet", async () => {
    await setup((c) => (c.path === "/api/v1/me/brief" ? json(404, { error: { code: "not_found", message: "no brief" } }) : undefined));
    expect(await screen.findByText(/You have no brief yet/)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  test("an error offers a retry that works", async () => {
    let fail = true;
    await setup((c) => (c.path === "/api/v1/me/brief" ? (fail ? json(500, { error: { code: "internal_error", message: "internal error" } }) : json(200, brief())) : undefined));
    expect(await screen.findByRole("alert", undefined, { timeout: 4000 })).toHaveTextContent("internal error");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText(/Start with checkout/)).toBeInTheDocument();
  });

  test("the brief the model wrote and task titles are text, never markup", async () => {
    const evil = '<img src=x onerror="alert(1)">';
    const { container } = await setup((c) =>
      c.path === "/api/v1/me/brief"
        ? json(200, brief({ text: evil, top_items: [{ ...brief().top_items[0], title: "<b>bold</b>" }] }))
        : undefined,
    );
    await screen.findByText(evil);
    expect(container.querySelector("img, b")).toBeNull();
    expect(screen.getByRole("link", { name: "<b>bold</b>" })).toBeInTheDocument();
  });
});
