import { api, unwrap, type Schemas } from "./client";

export type Task = Schemas["Task"];
export type TaskDetail = Schemas["TaskDetail"];
export type TaskStatus = Schemas["TaskStatus"];
export type Priority = Schemas["Priority"];
export type TaskList = Schemas["TaskList"];

export const STATUSES: TaskStatus[] = ["todo", "in_progress", "blocked", "completed", "archived"];
export const BOARD_STATUSES: TaskStatus[] = ["todo", "in_progress", "blocked", "completed"];
export const PRIORITIES: Priority[] = ["p1_critical", "p2_high", "p3_medium", "p4_low"];

export const STATUS_LABEL: Record<TaskStatus, string> = {
  todo: "To do",
  in_progress: "In progress",
  blocked: "Blocked",
  completed: "Completed",
  archived: "Archived",
};

export const PRIORITY_LABEL: Record<Priority, string> = {
  p1_critical: "P1 Critical",
  p2_high: "P2 High",
  p3_medium: "P3 Medium",
  p4_low: "P4 Low",
};

export interface TaskFilters {
  status?: TaskStatus;
  priority?: Priority;
  assignee_id?: string;
}

/** A page of tasks with the ETag that names it, kept to poll cheaply. */
export interface ListSnapshot {
  data: TaskList;
  etag: string | null;
}

/**
 * fetchTaskList reads one page. With the ETag of the last answer it sends
 * If-None-Match; a 304 returns null and the caller keeps what it has.
 */
export async function fetchTaskList(
  filters: TaskFilters,
  opts: { cursor?: string; etag?: string | null; limit?: number } = {},
): Promise<ListSnapshot | null> {
  const result = await api.GET("/api/v1/tasks", {
    params: {
      query: { ...filters, cursor: opts.cursor, limit: opts.limit ?? 50 },
      header: opts.etag ? { "If-None-Match": opts.etag } : {},
    },
    // The app does its own conditional requests; the browser's HTTP cache
    // would hide a 304 from it.
    cache: "no-store",
  });
  if (result.response.status === 304) return null;
  return { data: unwrap(result), etag: result.response.headers.get("ETag") };
}

export async function createTask(input: { workspace_id: string; title: string }): Promise<Task> {
  return unwrap(await api.POST("/api/v1/tasks", { body: input }));
}

/** updateTask sends a change with the version the user saw (If-Match). */
export async function updateTask(id: string, version: number, body: Schemas["TaskUpdate"]): Promise<Task> {
  return unwrap(
    await api.PATCH("/api/v1/tasks/{id}", {
      params: { path: { id }, header: { "If-Match": `"${version}"` } },
      body,
    }),
  );
}
