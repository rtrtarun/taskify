import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { afterEach, vi } from "vitest";
import type { InAppNotification } from "../../api/notifications";
import { SessionStore } from "../../auth/session";
import { json, meHandler, mockApi, renderWithProviders, signInAs, type Call } from "../../test-utils";
import NotificationsPage from "./NotificationsPage";

afterEach(() => vi.unstubAllGlobals());

const TASK = "0193a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b";

function note(over: Partial<InAppNotification> = {}): InAppNotification {
  return {
    id: "ntf-1", event_type: "task.updated", title: "Task updated", body: "Priority changed", created_at: "2026-11-20T10:00:00Z",
    task_id: TASK, job_id: null,
    task: { id: TASK, title: "Fix checkout", status: "todo", priority: "p3_medium", assignee_id: "usr_bob", labels: [] },
    read: false, signal: null, snoozed_until: null, ...over,
  };
}

const isFeed = (c: Call) => c.method === "GET" && c.path === "/api/v1/notifications/in-app";
const feed = (items: InAppNotification[], next: string | null = null) => json(200, { notifications: items, next_cursor: next });

async function setup(...handlers: Parameters<typeof mockApi>) {
  const api = mockApi(meHandler("member"), ...handlers);
  const store = new SessionStore();
  await signInAs(store);
  const utils = renderWithProviders(
    <Routes>
      <Route path="/notifications" element={<NotificationsPage pollMs={60_000} now={() => Date.parse("2026-11-21T00:00:00Z")} />} />
      <Route path="/tasks/:id" element={<h1>Task page</h1>} />
    </Routes>,
    { route: "/notifications", store },
  );
  return { api, ...utils };
}

describe("notifications panel", () => {
  test("shows notifications as text, with the task they are about", async () => {
    await setup((c) => (isFeed(c) ? feed([note({ title: "<i>Hi</i>", body: "<script>x()</script>" })]) : undefined));
    expect(await screen.findByRole("heading", { name: "<i>Hi</i>" })).toBeInTheDocument();
    expect(screen.getByText("<script>x()</script>")).toBeInTheDocument();
    expect(document.querySelector("i, script")).toBeNull();
    expect(screen.getByRole("link", { name: "Fix checkout" })).toHaveAttribute("href", `/tasks/${TASK}`);
  });

  test("loading and empty states", async () => {
    await setup((c) => (isFeed(c) ? feed([]) : undefined));
    expect(screen.getByRole("status")).toHaveTextContent("Loading notifications");
    expect(await screen.findByText("You have no notifications.")).toBeInTheDocument();
  });

  test("when the feed is unavailable the panel says so and offers a retry", async () => {
    let down = true;
    const { api } = await setup((c) => {
      if (!isFeed(c)) return undefined;
      return down
        ? json(503, { error: { code: "upstream_unavailable", message: "later" } }, { "Retry-After": "10" })
        : feed([note()]);
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("Notifications are unavailable right now. Your tasks are not affected");
    down = false;
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("heading", { name: "Task updated" })).toBeInTheDocument();
    expect(api.calls.filter(isFeed).length).toBe(2);
  });

  test("act acks as acted and opens the task", async () => {
    const { api } = await setup((c) => {
      if (c.method === "POST" && c.path === "/api/v1/notifications/ntf-1/ack") {
        return json(200, { notification_id: "ntf-1", signal: "acted", snoozed_until: null, acked_at: "2026-11-21T00:00:00Z" });
      }
      return isFeed(c) ? feed([note()]) : undefined;
    });
    await userEvent.click(await screen.findByRole("button", { name: "Open" }));
    expect(await screen.findByRole("heading", { name: "Task page" })).toBeInTheDocument();
    expect(api.calls.find((c) => c.method === "POST")!.body).toEqual({ signal: "acted" });
  });

  test("snooze sends a duration, dismiss sends the signal, and handled items disappear", async () => {
    let items = [note(), note({ id: "ntf-2", title: "Second" })];
    const { api } = await setup((c) => {
      if (c.method === "POST") {
        const id = c.path.split("/")[4];
        const body = c.body as { signal: "snoozed" | "dismissed" };
        items = items.map((n) =>
          n.id === id ? { ...n, read: true, signal: body.signal, snoozed_until: body.signal === "snoozed" ? "2026-11-21T02:00:00Z" : null } : n,
        );
        return json(200, { notification_id: id, signal: body.signal, snoozed_until: null, acked_at: "2026-11-21T00:00:00Z" });
      }
      return isFeed(c) ? feed(items) : undefined;
    });
    await screen.findByRole("heading", { name: "Task updated" });
    await userEvent.click(screen.getAllByRole("button", { name: "Snooze 2h" })[0]);
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Task updated" })).not.toBeInTheDocument());
    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Second" })).not.toBeInTheDocument());
    const posts = api.calls.filter((c) => c.method === "POST").map((c) => c.body);
    expect(posts).toEqual([{ signal: "snoozed", duration: "2h" }, { signal: "dismissed" }]);
    expect(await screen.findByText(/Everything here is handled/)).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText("Show handled"));
    expect(await screen.findByRole("heading", { name: "Task updated" })).toBeInTheDocument();
  });

  test("a snooze that has ended shows the notification again", async () => {
    await setup((c) => (isFeed(c) ? feed([note({ read: true, signal: "snoozed", snoozed_until: "2026-11-20T12:00:00Z" })]) : undefined));
    expect(await screen.findByRole("heading", { name: "Task updated" })).toBeInTheDocument();
  });

  test("load more fetches the next page by cursor", async () => {
    const { api } = await setup((c) => {
      if (!isFeed(c)) return undefined;
      return c.url.searchParams.get("cursor") === "20" ? feed([note({ id: "ntf-9", title: "Older" })]) : feed([note()], "20");
    });
    await userEvent.click(await screen.findByRole("button", { name: "Load more" }));
    expect(await screen.findByRole("heading", { name: "Older" })).toBeInTheDocument();
    expect(api.calls.filter(isFeed).map((c) => c.url.searchParams.get("cursor"))).toEqual([null, "20"]);
  });
});
