import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api/client";
import {
  applyJob,
  decomposeTask,
  dismissJob,
  fetchAgentRuns,
  fetchJobs,
  fetchMyBrief,
  fetchSubtasks,
  type AgentJob,
  type JobsSnapshot,
} from "../api/agents";
import { pollTask, type TaskDetail } from "../api/tasks";

export const AGENT_POLL_MS = 30_000;
export const APPROVALS_POLL_MS = 20_000;
export const JOB_POLL_MS = 2_000;

const noRetryOnClientError = (n: number, err: unknown) => !(err instanceof ApiError && err.status < 500) && n < 2;

export function useSubtasks(taskId: string, pollMs: number = AGENT_POLL_MS) {
  return useQuery({
    queryKey: ["tasks", "subtasks", taskId],
    queryFn: () => fetchSubtasks(taskId),
    refetchInterval: pollMs,
    retry: noRetryOnClientError,
  });
}

export function useAgentRuns(taskId: string, pollMs: number = AGENT_POLL_MS) {
  return useQuery({
    queryKey: ["tasks", "agent-runs", taskId],
    queryFn: () => fetchAgentRuns(taskId),
    refetchInterval: pollMs,
    retry: noRetryOnClientError,
  });
}

/** A job that is still going: the page keeps asking until it stops. */
export const jobIsOpen = (j: AgentJob | undefined): boolean => j?.status === "pending" || j?.status === "running";

/**
 * useDecomposeJob follows the task's latest decomposer job, so a page that
 * is opened (or reloaded) mid-run shows its progress. While the job is
 * pending or running it asks every two seconds; when it completes, the
 * subtasks and the task are read again.
 */
export function useDecomposeJob(taskId: string, pollMs: number = JOB_POLL_MS) {
  const qc = useQueryClient();
  const key = ["agent-jobs", "decompose", taskId] as const;
  return useQuery({
    queryKey: key,
    queryFn: async (): Promise<AgentJob | null> => {
      const previous = qc.getQueryData<AgentJob | null>(key);
      const snap = await fetchJobs({ task_id: taskId, agent_type: "decomposer" }, { limit: 1 });
      const job = snap?.data.agent_jobs[0] ?? null;
      if (job && previous && previous.id === job.id && previous.status !== job.status) {
        // The timeline lists the job's turns; read it again as the job moves.
        void qc.invalidateQueries({ queryKey: ["tasks", "agent-runs", taskId] });
        if (job.status === "completed") {
          void qc.invalidateQueries({ queryKey: ["tasks", "subtasks", taskId] });
          void qc.invalidateQueries({ queryKey: ["tasks", "detail", taskId] });
        }
      }
      return job;
    },
    refetchInterval: (q) => (jobIsOpen(q.state.data ?? undefined) ? pollMs : false),
    retry: noRetryOnClientError,
  });
}

export function useDecompose(taskId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => decomposeTask(taskId),
    onSuccess: (job) => qc.setQueryData(["agent-jobs", "decompose", taskId], job),
  });
}

/**
 * useAwaitingApprovals reads the proposals waiting for the caller. The
 * server decides which jobs the caller may see. It polls with If-None-Match,
 * so an unchanged inbox answers 304.
 */
export function useAwaitingApprovals(pollMs: number = APPROVALS_POLL_MS) {
  const qc = useQueryClient();
  const key = ["agent-jobs", "awaiting"] as const;
  return useQuery({
    queryKey: key,
    refetchInterval: pollMs,
    queryFn: async (): Promise<JobsSnapshot> => {
      const prev = qc.getQueryData<JobsSnapshot>(key);
      const next = await fetchJobs({ status: "awaiting_approval" }, { etag: prev?.etag });
      if (next) return next;
      if (prev) return prev;
      return (await fetchJobs({ status: "awaiting_approval" }))!;
    },
  });
}

/** useTaskNow reads a task for a card that shows its current values. */
export function useTaskNow(taskId: string | null) {
  return useQuery({
    queryKey: ["tasks", "detail", taskId],
    enabled: taskId !== null,
    queryFn: async (): Promise<TaskDetail> => (await pollTask(taskId!, null))!.data,
    retry: noRetryOnClientError,
  });
}

export type Decision = "approve" | "reject";

export function useDecideJob() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { id: string; decision: Decision }) => (v.decision === "approve" ? applyJob(v.id) : dismissJob(v.id)),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ["agent-jobs"] });
      void qc.invalidateQueries({ queryKey: ["tasks"] });
    },
  });
}

export function useBrief() {
  return useQuery({
    queryKey: ["me", "brief"],
    queryFn: fetchMyBrief,
    // No brief yet is 404, which asking again will not change by itself.
    retry: noRetryOnClientError,
  });
}
