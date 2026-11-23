import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, type Schemas } from "../api/client";
import { addComment, archiveTask, completeTask, pollTask, restoreTask, updateTask, type Task, type TaskDetail } from "../api/tasks";

export const DETAIL_POLL_MS = 30_000;

const detailKey = (id: string) => ["tasks", "detail", id] as const;

// The ETag of the detail the cache holds, kept beside it: the cached value is
// a plain TaskDetail that edits update in place.
const etagKey = (id: string) => ["tasks", "detail-etag", id] as const;

/**
 * useTaskDetail reads a task and polls it with If-None-Match: an unchanged
 * task answers 304 and the cached detail stays.
 */
export function useTaskDetail(id: string, pollMs: number = DETAIL_POLL_MS) {
  const qc = useQueryClient();
  return useQuery({
    queryKey: detailKey(id),
    queryFn: async (): Promise<TaskDetail> => {
      const prev = qc.getQueryData<TaskDetail>(detailKey(id));
      const tag = prev ? (qc.getQueryData<string | null>(etagKey(id)) ?? null) : null;
      const next = await pollTask(id, tag);
      if (!next) return prev!; // 304: nothing changed (a tag is only sent with a cached detail)
      qc.setQueryData(etagKey(id), next.etag);
      return next.data;
    },
    refetchInterval: pollMs,
    // A missing or foreign task will not appear by asking again.
    retry: (n, err) => !(err instanceof ApiError && err.status < 500) && n < 2,
  });
}

/** Merges a server task into the cached detail, keeping its comments. */
function withTask(old: TaskDetail | undefined, task: Task): TaskDetail | undefined {
  return old ? { ...old, ...task } : old;
}

export interface UpdateVars {
  changes: Schemas["TaskUpdate"];
  /** The version the user was looking at, sent as If-Match. */
  version: number;
}

/**
 * useUpdateTask is the edit flow of architecture section 9.6: the change is
 * shown at once; if the server answers 409 (someone else changed the task
 * first) the screen rolls back to what it had, reloads the latest task, and
 * the caller tells the user their change was not saved. Any other failure
 * rolls back too.
 */
export function useUpdateTask(id: string) {
  const qc = useQueryClient();
  const key = detailKey(id);
  return useMutation({
    mutationFn: (v: UpdateVars) => updateTask(id, v.version, v.changes),
    onMutate: async (v) => {
      await qc.cancelQueries({ queryKey: key });
      const previous = qc.getQueryData<TaskDetail>(key);
      // Only the fields that are shown as they are sent; the server's answer
      // replaces this on success.
      const { version: _v, ...fields } = v.changes as Record<string, unknown>;
      void _v;
      if (previous) qc.setQueryData<TaskDetail>(key, { ...previous, ...(fields as Partial<TaskDetail>) });
      return { previous };
    },
    onError: (err, _v, ctx) => {
      if (ctx?.previous) qc.setQueryData(key, ctx.previous);
      // On 409 the cached copy is stale: reload the latest.
      if (err instanceof ApiError && err.code === "version_conflict") void qc.invalidateQueries({ queryKey: key });
    },
    onSuccess: (task) => qc.setQueryData<TaskDetail>(key, (old) => withTask(old, task)),
    onSettled: () => qc.invalidateQueries({ queryKey: ["tasks", "list"] }),
  });
}

/** Complete, archive, and restore share a shape: act with the seen version, then adopt the answer. */
function useTaskAction(id: string, act: (id: string, version: number) => Promise<Task>) {
  const qc = useQueryClient();
  const key = detailKey(id);
  return useMutation({
    mutationFn: (version: number) => act(id, version),
    onSuccess: (task) => qc.setQueryData<TaskDetail>(key, (old) => withTask(old, task)),
    onError: (err) => {
      if (err instanceof ApiError && err.code === "version_conflict") void qc.invalidateQueries({ queryKey: key });
    },
    onSettled: () => qc.invalidateQueries({ queryKey: ["tasks", "list"] }),
  });
}

export const useCompleteTask = (id: string) => useTaskAction(id, completeTask);
export const useArchiveTask = (id: string) => useTaskAction(id, archiveTask);
export const useRestoreTask = (id: string) => useTaskAction(id, restoreTask);

export function useAddComment(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: string) => addComment(id, body),
    onSuccess: () => qc.invalidateQueries({ queryKey: detailKey(id) }),
  });
}
