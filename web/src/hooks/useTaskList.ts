import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createTask, fetchTaskList, updateTask, type ListSnapshot, type TaskFilters, type TaskStatus } from "../api/tasks";

/** How often the open list asks whether anything changed (15 to 30 s). */
export const POLL_MS = 20_000;

/**
 * useTaskList reads the first page of a filtered list and keeps it current by
 * polling with If-None-Match: an unchanged list answers 304 and costs the
 * server one small query. Later pages are fetched on demand by the page.
 */
export function useTaskList(filters: TaskFilters, pollMs: number = POLL_MS) {
  const qc = useQueryClient();
  const key = ["tasks", "list", filters] as const;
  return useQuery({
    queryKey: key,
    refetchInterval: pollMs,
    queryFn: async (): Promise<ListSnapshot> => {
      const prev = qc.getQueryData<ListSnapshot>(key);
      const next = await fetchTaskList(filters, { etag: prev?.etag });
      if (next) return next;
      if (prev) return prev; // 304: nothing changed
      return (await fetchTaskList(filters))!; // a 304 with nothing held cannot be used
    },
  });
}

export function useCreateTask() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: createTask,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["tasks"] }),
  });
}

/** useMoveTask changes a task's status; the server decides whether it may. */
export function useMoveTask() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { id: string; version: number; status: TaskStatus }) => updateTask(v.id, v.version, { status: v.status }),
    onSettled: () => qc.invalidateQueries({ queryKey: ["tasks"] }),
  });
}
