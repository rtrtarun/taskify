import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, vi } from "vitest";
import type { Task } from "../../api/tasks";
import { SessionStore } from "../../auth/session";
import { json, meHandler, mockApi, renderWithProviders, signInAs, type Call } from "../../test-utils";
import TasksPage from "./TasksPage";

afterEach(() => vi.unstubAllGlobals());

function task(over: Partial<Task> = {}): Task {
  return {
    id: "0193a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b", tenant_id: "tenant_acme", workspace_id: "ws_core", title: "Fix checkout",
    description: "", status: "todo", priority: "p3_medium", creator_id: "usr_bob", assignee_id: "usr_bob", labels: [], metadata: {},
    watcher_ids: [], due_at: null, remind_at: null, completed_at: null, archived_at: null, blocked_reason: null, version: 1,
    created_at: "2026-11-20T10:00:00Z", updated_at: "2026-11-20T10:00:00Z", ...over,
  } as Task;
}

const list = (tasks: Task[], etag = 'W/"a"', next: string | null = null) =>
  json(200, { tasks, next_cursor: next }, { ETag: etag });

async function setup(route: string, ...handlers: Parameters<typeof mockApi>) {
  const api = mockApi(meHandler("member"), ...handlers);
  const store = new SessionStore();
  await signInAs(store); // the app only shows this page to a signed-in user
  const utils = renderWithProviders(<TasksPage pollMs={50} />, { route, store });
  return { api, ...utils };
}

const isList = (c: Call) => c.method === "GET" && c.path === "/api/v1/tasks";

describe("task list states", () => {
  test("loading, then the tasks, rendered as text", async () => {
    const evil = task({ title: '<img src=x onerror="alert(1)">' });
    await setup("/tasks", (c) => (isList(c) ? list([evil, task({ id: "0193a1b2-c3d4-7e5f-8a6b-000000000002", title: "Second" })]) : undefined));
    expect(screen.getByRole("status")).toHaveTextContent("Loading tasks");
    expect(await screen.findByText('<img src=x onerror="alert(1)">')).toBeInTheDocument();
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getByRole("link", { name: "Second" })).toHaveAttribute("href", "/tasks/0193a1b2-c3d4-7e5f-8a6b-000000000002");
  });

  test("empty", async () => {
    await setup("/tasks", (c) => (isList(c) ? list([]) : undefined));
    expect(await screen.findByText(/No tasks yet/)).toBeInTheDocument();
  });

  test("empty with filters says so", async () => {
    await setup("/tasks?priority=p1_critical", (c) => (isList(c) ? list([]) : undefined));
    expect(await screen.findByText(/No tasks match these filters/)).toBeInTheDocument();
  });

  test("error, with a retry that works", async () => {
    let fail = true;
    await setup("/tasks", (c) => {
      if (!isList(c)) return undefined;
      return fail ? json(500, { error: { code: "internal_error", message: "internal error" } }) : list([task()]);
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("internal error");
    fail = false;
    await userEvent.click(await screen.findByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("link", { name: "Fix checkout" })).toBeInTheDocument();
  });

  test("filters go to the server", async () => {
    const { api } = await setup("/tasks?status=blocked&priority=p1_critical&mine=1", (c) => (isList(c) ? list([]) : undefined));
    await screen.findByText(/No tasks match/);
    const q = api.calls.find(isList)!.url.searchParams;
    expect([q.get("status"), q.get("priority"), q.get("assignee_id")]).toEqual(["blocked", "p1_critical", "usr_bob"]);
  });

  test("load more appends the next page", async () => {
    await setup("/tasks", (c) => {
      if (!isList(c)) return undefined;
      return c.url.searchParams.get("cursor")
        ? list([task({ id: "0193a1b2-c3d4-7e5f-8a6b-000000000003", title: "Older" })])
        : list([task()], 'W/"a"', "next1");
    });
    await userEvent.click(await screen.findByRole("button", { name: "Load more" }));
    expect(await screen.findByRole("link", { name: "Older" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Fix checkout" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
  });
});

describe("polling", () => {
  test("a repeat poll sends If-None-Match, and a 304 keeps the list", async () => {
    const { api } = await setup("/tasks", (c) => {
      if (!isList(c)) return undefined;
      return c.headers.get("If-None-Match") === 'W/"a"' ? json(304) : list([task()]);
    });
    await screen.findByRole("link", { name: "Fix checkout" });
    await waitFor(() => expect(api.calls.filter(isList).length).toBeGreaterThanOrEqual(3));
    const polls = api.calls.filter(isList);
    expect(polls[0].headers.get("If-None-Match")).toBeNull();
    expect(polls.slice(1).every((c) => c.headers.get("If-None-Match") === 'W/"a"')).toBe(true);
    expect(screen.getByRole("link", { name: "Fix checkout" })).toBeInTheDocument();
  });

  test("a changed list replaces the old one", async () => {
    let n = 0;
    await setup("/tasks", (c) => (isList(c) ? (n++ === 0 ? list([task()]) : list([task({ title: "Renamed" })], 'W/"b"')) : undefined));
    expect(await screen.findByRole("link", { name: "Renamed" })).toBeInTheDocument();
  });

  test("a failed refresh keeps the last list on screen", async () => {
    let n = 0;
    await setup("/tasks", (c) => (isList(c) ? (n++ === 0 ? list([task()]) : json(500, { error: { code: "internal_error", message: "x" } })) : undefined));
    await screen.findByRole("link", { name: "Fix checkout" });
    expect(await screen.findByText(/Could not refresh/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Fix checkout" })).toBeInTheDocument();
  });
});

describe("quick add", () => {
  test("creates a task in the workspace of the list and refreshes", async () => {
    let created = false;
    const { api } = await setup("/tasks", (c) => {
      if (c.method === "POST" && c.path === "/api/v1/tasks") {
        created = true;
        return json(201, task({ title: "New one" }));
      }
      return isList(c) ? list(created ? [task(), task({ id: "0193a1b2-c3d4-7e5f-8a6b-000000000009", title: "New one" })] : [task()]) : undefined;
    });
    await screen.findByRole("link", { name: "Fix checkout" });
    expect(screen.getByLabelText("Workspace")).toHaveValue("ws_core");
    await userEvent.type(screen.getByLabelText("Title"), "New one");
    await userEvent.click(screen.getByRole("button", { name: "Add task" }));
    expect(await screen.findByRole("link", { name: "New one" })).toBeInTheDocument();
    expect(api.calls.find((c) => c.method === "POST")!.body).toEqual({ workspace_id: "ws_core", title: "New one" });
    expect(screen.getByLabelText("Title")).toHaveValue("");
  });

  test("shows the server's message when the task is refused", async () => {
    await setup("/tasks", (c) => {
      if (c.method === "POST") return json(422, { error: { code: "validation_failed", message: "workspace ws_x does not exist" } });
      return isList(c) ? list([]) : undefined;
    });
    await screen.findByText(/No tasks yet/);
    await userEvent.type(screen.getByLabelText("Title"), "T");
    await userEvent.type(screen.getByLabelText("Workspace"), "ws_x");
    await userEvent.click(screen.getByRole("button", { name: "Add task" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("workspace ws_x does not exist");
  });
});

describe("board", () => {
  const two = [task(), task({ id: "0193a1b2-c3d4-7e5f-8a6b-000000000002", title: "Working", status: "in_progress" })];

  test("a column per status holds its tasks", async () => {
    await setup("/tasks?view=board", (c) => (isList(c) ? list(two) : undefined));
    const todo = (await screen.findByRole("heading", { name: /To do/ })).closest("section")!;
    const doing = screen.getByRole("heading", { name: /In progress/ }).closest("section")!;
    expect(within(todo).getByText("Fix checkout")).toBeInTheDocument();
    expect(within(doing).getByText("Working")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: /Blocked/ })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: /Completed/ })).toBeInTheDocument();
  });

  test("the Move to control changes status with the version the user saw", async () => {
    const { api } = await setup("/tasks?view=board", (c) => {
      if (c.method === "PATCH") return json(200, task({ status: "in_progress", version: 2 }));
      return isList(c) ? list(two) : undefined;
    });
    await userEvent.selectOptions(await screen.findByRole("combobox", { name: "Move Fix checkout to" }), "in_progress");
    await waitFor(() => expect(api.calls.some((c) => c.method === "PATCH")).toBe(true));
    const patch = api.calls.find((c) => c.method === "PATCH")!;
    expect(patch.body).toEqual({ status: "in_progress" });
    expect(patch.headers.get("If-Match")).toBe('"1"');
  });

  test("dragging a card to a column moves it", async () => {
    const { api } = await setup("/tasks?view=board", (c) => {
      if (c.method === "PATCH") return json(200, task({ status: "completed" }));
      return isList(c) ? list(two) : undefined;
    });
    const card = (await screen.findByText("Fix checkout")).closest("li")!;
    const target = screen.getByRole("heading", { name: /Completed/ }).closest("section")!;
    const data: Record<string, string> = {};
    const dataTransfer = { setData: (k: string, v: string) => (data[k] = v), getData: (k: string) => data[k] };
    fireEvent.dragStart(card, { dataTransfer });
    fireEvent.dragOver(target, { dataTransfer });
    fireEvent.drop(target, { dataTransfer });
    await waitFor(() => expect(api.calls.find((c) => c.method === "PATCH")?.body).toEqual({ status: "completed" }));
  });

  test("a disallowed move shows the server's message", async () => {
    await setup("/tasks?view=board", (c) => {
      if (c.method === "PATCH") {
        return json(422, { error: { code: "invalid_transition", message: "cannot move a task from todo to blocked without a reason" } });
      }
      return isList(c) ? list(two) : undefined;
    });
    await userEvent.selectOptions(await screen.findByRole("combobox", { name: "Move Fix checkout to" }), "blocked");
    expect(await screen.findByRole("alert")).toHaveTextContent("cannot move a task from todo to blocked without a reason");
    await act(async () => {});
  });
});
