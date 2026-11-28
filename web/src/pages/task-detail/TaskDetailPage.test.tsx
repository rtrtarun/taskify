import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { afterEach, vi } from "vitest";
import type { TaskDetail } from "../../api/tasks";
import { SessionStore } from "../../auth/session";
import { json, meHandler, mockApi, renderWithProviders, signInAs, type Call } from "../../test-utils";
import TaskDetailPage from "./TaskDetailPage";

afterEach(() => vi.unstubAllGlobals());

const ID = "0193a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b";

function detail(over: Partial<TaskDetail> = {}): TaskDetail {
  return {
    id: ID, tenant_id: "tenant_acme", workspace_id: "ws_core", title: "Fix checkout", description: "It crashes",
    status: "todo", priority: "p3_medium", creator_id: "usr_bob", assignee_id: "usr_bob", labels: ["bug"], metadata: {},
    watcher_ids: [], due_at: null, remind_at: null, completed_at: null, archived_at: null, blocked_reason: null, version: 1,
    created_at: "2026-11-20T10:00:00Z", updated_at: "2026-11-20T10:00:00Z", comments: [], ...over,
  } as TaskDetail;
}

const isGet = (c: Call) => c.method === "GET" && c.path === `/api/v1/tasks/${ID}`;
const isPatch = (c: Call) => c.method === "PATCH" && c.path === `/api/v1/tasks/${ID}`;

async function setup(...handlers: Parameters<typeof mockApi>) {
  const api = mockApi(meHandler("member"), ...handlers);
  const store = new SessionStore();
  await signInAs(store);
  const utils = renderWithProviders(
    <Routes>
      <Route path="/tasks/:id" element={<TaskDetailPage />} />
    </Routes>,
    { route: `/tasks/${ID}`, store },
  );
  return { api, ...utils };
}

describe("task detail", () => {
  test("shows the task and its comments as text", async () => {
    await setup((c) =>
      isGet(c)
        ? json(200, detail({
            title: "<b>bold</b> title",
            comments: [{ id: "c1", task_id: ID, author_id: "agent:triage", body: "<script>alert(1)</script>\nline two", mentions: [], created_at: "2026-11-20T11:00:00Z" }],
          }))
        : undefined,
    );
    expect(await screen.findByRole("heading", { name: "<b>bold</b> title" })).toBeInTheDocument();
    expect(screen.getByText(/<script>alert\(1\)<\/script>/)).toBeInTheDocument();
    expect(document.querySelector("script, b")).toBeNull();
    expect(screen.getByText("agent:triage")).toBeInTheDocument();
  });

  test("a task that is not there, or is another tenant's, is not shown", async () => {
    await setup((c) => (isGet(c) ? json(404, { error: { code: "not_found", message: "not found" } }) : undefined));
    expect(await screen.findByRole("alert")).toHaveTextContent("does not exist, or you cannot see it");
  });

  test("an error offers a retry", async () => {
    let fail = true;
    await setup((c) => (isGet(c) ? (fail ? json(500, { error: { code: "internal_error", message: "internal error" } }) : json(200, detail())) : undefined));
    await screen.findByRole("alert", undefined, { timeout: 4000 });
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("heading", { name: "Fix checkout" })).toBeInTheDocument();
  });

  test("a priority change shows at once and is sent with the version seen", async () => {
    let release: (r: Response) => void = () => {};
    const { api } = await setup((c) => {
      if (isGet(c)) return json(200, detail());
      if (isPatch(c)) return new Promise<Response>((r) => (release = r)) as unknown as Response;
      return undefined;
    });
    await screen.findByRole("heading", { name: "Fix checkout" });
    await userEvent.selectOptions(screen.getByLabelText("Priority"), "p1_critical");
    // Optimistic: the select already shows the new value while the request is open.
    expect(screen.getByLabelText("Priority")).toHaveValue("p1_critical");
    release(json(200, detail({ priority: "p1_critical", version: 2 })));
    await waitFor(() => expect(screen.getByText(/version 2/)).toBeInTheDocument());
    const patch = api.calls.find(isPatch)!;
    expect(patch.body).toEqual({ priority: "p1_critical" });
    expect(patch.headers.get("If-Match")).toBe('"1"');
  });

  test("saving the form sends only what changed", async () => {
    const { api } = await setup((c) => (isGet(c) ? json(200, detail()) : isPatch(c) ? json(200, detail({ title: "Fix it", version: 2 })) : undefined));
    const title = await screen.findByLabelText("Title");
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
    await userEvent.clear(title);
    await userEvent.type(title, "Fix it");
    await userEvent.type(screen.getByLabelText("Labels (comma separated)"), ", ui");
    await userEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(api.calls.some(isPatch)).toBe(true));
    expect(api.calls.find(isPatch)!.body).toEqual({ title: "Fix it", labels: ["bug", "ui"] });
  });

  test("a conflict rolls back, reloads, says the change was not saved, and keeps what the user typed visible", async () => {
    let served = 0;
    const { api } = await setup((c) => {
      if (isGet(c)) return json(200, served++ === 0 ? detail() : detail({ priority: "p4_low", version: 2, title: "Changed by someone" }));
      if (isPatch(c)) return json(409, { error: { code: "version_conflict", message: "stale", details: { current_version: 2 } } });
      return undefined;
    });
    await screen.findByRole("heading", { name: "Fix checkout" });
    await userEvent.selectOptions(screen.getByLabelText("Priority"), "p1_critical");

    const notice = await screen.findByRole("alert");
    expect(notice).toHaveTextContent("your change was not saved");
    expect(notice).toHaveTextContent("priority: p1_critical"); // nothing lost silently
    // Rolled back and reloaded: the latest task is shown, not the optimistic value.
    expect(await screen.findByRole("heading", { name: "Changed by someone" })).toBeInTheDocument();
    expect(screen.getByLabelText("Priority")).toHaveValue("p4_low");
    expect(api.calls.filter(isGet).length).toBe(2);
  });

  test("a refused change is rolled back with the server's message", async () => {
    await setup((c) => {
      if (isGet(c)) return json(200, detail());
      if (isPatch(c)) return json(422, { error: { code: "invalid_transition", message: "cannot go from todo to completed this way" } });
      return undefined;
    });
    await screen.findByRole("heading", { name: "Fix checkout" });
    await userEvent.selectOptions(screen.getByLabelText("Status"), "completed");
    expect(await screen.findByRole("alert")).toHaveTextContent("cannot go from todo to completed this way");
    expect(screen.getByLabelText("Status")).toHaveValue("todo");
  });

  test("adds a comment and clears the box", async () => {
    let posted = false;
    const { api } = await setup((c) => {
      if (c.method === "POST" && c.path === `/api/v1/tasks/${ID}/comments`) {
        posted = true;
        return json(201, { id: "c9", task_id: ID, author_id: "usr_bob", body: "Looking now", mentions: [], created_at: "2026-11-20T12:00:00Z" });
      }
      if (isGet(c)) {
        return json(200, detail({ comments: posted ? [{ id: "c9", task_id: ID, author_id: "usr_bob", body: "Looking now", mentions: [], created_at: "2026-11-20T12:00:00Z" }] : [] }));
      }
      return undefined;
    });
    expect(await screen.findByText("No comments yet.")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("Comment"), "Looking now");
    await userEvent.click(screen.getByRole("button", { name: "Add comment" }));
    expect(await screen.findByText("Looking now")).toBeInTheDocument();
    expect(api.calls.find((c) => c.path.endsWith("/comments"))!.body).toEqual({ body: "Looking now" });
    expect(screen.getByLabelText("Comment")).toHaveValue("");
  });

  test("complete, archive, and restore send the seen version", async () => {
    let task = detail();
    const { api } = await setup((c) => {
      if (isGet(c)) return json(200, task);
      if (c.method === "POST" && c.path.endsWith("/complete")) return json(200, (task = detail({ status: "completed", version: 2 })));
      if (c.method === "DELETE") return json(200, (task = detail({ status: "archived", version: 3 })));
      if (c.method === "POST" && c.path.endsWith("/restore")) return json(200, (task = detail({ status: "todo", version: 4 })));
      return undefined;
    });
    await userEvent.click(await screen.findByRole("button", { name: "Complete" }));
    await screen.findByText(/version 2/);
    expect(screen.queryByRole("button", { name: "Complete" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Archive" }));
    await userEvent.click(await screen.findByRole("button", { name: "Restore" }));
    await screen.findByText(/version 4/);
    const versions = api.calls.filter((c) => c.method !== "GET" && c.path.startsWith("/api/v1/tasks/")).map((c) => c.headers.get("If-Match"));
    expect(versions).toEqual(['"1"', '"2"', '"3"']);
  });
});
