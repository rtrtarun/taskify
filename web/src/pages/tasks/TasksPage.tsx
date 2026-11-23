import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";
import { useSearchParams } from "react-router-dom";
import { fetchTaskList, PRIORITIES, PRIORITY_LABEL, STATUSES, STATUS_LABEL, type Priority, type Task, type TaskFilters, type TaskStatus } from "../../api/tasks";
import ErrorMessage, { describeError } from "../../components/ErrorMessage";
import { useAuth } from "../../auth/AuthProvider";
import { useCreateTask, useMoveTask, useTaskList } from "../../hooks/useTaskList";
import TaskBoard from "./TaskBoard";
import TaskList from "./TaskList";

function filtersFrom(params: URLSearchParams, userId: string | undefined): TaskFilters {
  const f: TaskFilters = {};
  const status = params.get("status");
  const priority = params.get("priority");
  if (status && (STATUSES as string[]).includes(status)) f.status = status as TaskStatus;
  if (priority && (PRIORITIES as string[]).includes(priority)) f.priority = priority as Priority;
  if (params.get("mine") === "1" && userId) f.assignee_id = userId;
  return f;
}

export default function TasksPage({ pollMs }: { pollMs?: number }) {
  const { identity } = useAuth();
  const [params, setParams] = useSearchParams();
  const view = params.get("view") === "board" ? "board" : "list";
  const filters = filtersFrom(params, identity?.userId);
  const list = useTaskList(filters, pollMs);
  const qc = useQueryClient();

  // Pages after the first, fetched on request and dropped when the filters change.
  const filterKey = JSON.stringify(filters);
  const [more, setMore] = useState<{ key: string; tasks: Task[]; cursor: string | null } | null>(null);
  const [moreError, setMoreError] = useState<unknown>(null);
  const extra = more && more.key === filterKey ? more : null;

  const first = list.data?.data;
  const cursor = extra ? extra.cursor : (first?.next_cursor ?? null);
  const tasks = (() => {
    if (!first) return [];
    const seen = new Set(first.tasks.map((t) => t.id));
    return [...first.tasks, ...(extra?.tasks ?? []).filter((t) => !seen.has(t.id))];
  })();

  async function loadMore() {
    if (!cursor) return;
    setMoreError(null);
    try {
      const page = await fetchTaskList(filters, { cursor });
      if (!page) return;
      setMore({ key: filterKey, tasks: [...(extra?.tasks ?? []), ...page.data.tasks], cursor: page.data.next_cursor });
    } catch (e) {
      setMoreError(e);
    }
  }

  function setParam(name: string, value: string) {
    const next = new URLSearchParams(params);
    if (value) next.set(name, value);
    else next.delete(name);
    setParams(next, { replace: true });
  }

  // Quick add needs a workspace; there is no workspace list, so it starts as
  // the workspace of a task already on screen and can be typed.
  const [title, setTitle] = useState("");
  const [workspace, setWorkspace] = useState("");
  const defaultWorkspace = first?.tasks[0]?.workspace_id ?? "";
  useEffect(() => {
    setWorkspace((w) => w || defaultWorkspace);
  }, [defaultWorkspace]);
  const create = useCreateTask();
  const move = useMoveTask();

  function add(e: FormEvent) {
    e.preventDefault();
    create.mutate(
      { workspace_id: workspace.trim(), title: title.trim() },
      { onSuccess: () => setTitle("") },
    );
  }

  return (
    <main>
      <h1>Tasks</h1>

      <form onSubmit={add} className="quick-add" aria-label="Add a task">
        <label>
          Title
          <input value={title} onChange={(e) => setTitle(e.target.value)} required maxLength={200} />
        </label>
        <label>
          Workspace
          <input value={workspace} onChange={(e) => setWorkspace(e.target.value)} required />
        </label>
        <button type="submit" disabled={create.isPending || title.trim() === "" || workspace.trim() === ""}>
          {create.isPending ? "Adding..." : "Add task"}
        </button>
      </form>
      {create.isError && <ErrorMessage error={create.error} />}

      <div className="filters" role="group" aria-label="Filters">
        <label>
          Status
          <select value={params.get("status") ?? ""} onChange={(e) => setParam("status", e.target.value)}>
            <option value="">Open (all but archived)</option>
            {STATUSES.map((s) => (
              <option key={s} value={s}>
                {STATUS_LABEL[s]}
              </option>
            ))}
          </select>
        </label>
        <label>
          Priority
          <select value={params.get("priority") ?? ""} onChange={(e) => setParam("priority", e.target.value)}>
            <option value="">Any</option>
            {PRIORITIES.map((p) => (
              <option key={p} value={p}>
                {PRIORITY_LABEL[p]}
              </option>
            ))}
          </select>
        </label>
        <label>
          <input type="checkbox" checked={params.get("mine") === "1"} onChange={(e) => setParam("mine", e.target.checked ? "1" : "")} />
          Assigned to me
        </label>
        <div role="group" aria-label="View">
          <button type="button" aria-pressed={view === "list"} onClick={() => setParam("view", "")}>
            List
          </button>
          <button type="button" aria-pressed={view === "board"} onClick={() => setParam("view", "board")}>
            Board
          </button>
        </div>
      </div>

      {move.isError && <ErrorMessage error={move.error} />}

      {list.isPending && <p role="status">Loading tasks...</p>}
      {list.isError && !first && <ErrorMessage error={list.error} onRetry={() => void list.refetch()} />}
      {list.isError && first && <p role="status">Could not refresh: {describeError(list.error)} Showing the last list.</p>}

      {first && tasks.length === 0 && (
        <p>{Object.keys(filters).length > 0 ? "No tasks match these filters." : "No tasks yet. Add the first one above."}</p>
      )}
      {first && tasks.length > 0 && (
        <>
          {view === "board" ? (
            <TaskBoard tasks={tasks} onMove={(task, status) => move.mutate({ id: task.id, version: task.version, status })} />
          ) : (
            <TaskList tasks={tasks} />
          )}
          {cursor && (
            <button type="button" onClick={() => void loadMore()}>
              Load more
            </button>
          )}
          {moreError != null && <ErrorMessage error={moreError} />}
        </>
      )}
      <button type="button" className="link" onClick={() => void qc.invalidateQueries({ queryKey: ["tasks"] })}>
        Refresh
      </button>
    </main>
  );
}
