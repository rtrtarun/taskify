import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api/client";
import {
  bootstrapRules,
  decideSuggestion,
  fetchJobRuns,
  fetchOutbox,
  fetchSuggestions,
  fetchWorkspaceSettings,
  replayOutbox,
  updateWorkspaceSettings,
  type AgentSettingsPatch,
  type OutboxStatus,
} from "../api/admin";
import { fetchJobs, type AgentJobStatus } from "../api/agents";

const noRetryOnClientError = (n: number, err: unknown) => !(err instanceof ApiError && err.status < 500) && n < 2;

export function useSuggestions() {
  return useQuery({ queryKey: ["admin", "suggestions"], queryFn: fetchSuggestions, retry: noRetryOnClientError });
}

export function useDecideSuggestion() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { id: string; decision: "approve" | "reject" }) => decideSuggestion(v.id, v.decision),
    onSettled: () => void qc.invalidateQueries({ queryKey: ["admin", "suggestions"] }),
  });
}

export function useOutbox(status: OutboxStatus | undefined) {
  return useQuery({ queryKey: ["admin", "outbox", status ?? "all"], queryFn: () => fetchOutbox(status), retry: noRetryOnClientError });
}

export function useReplay() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: replayOutbox,
    onSettled: () => void qc.invalidateQueries({ queryKey: ["admin", "outbox"] }),
  });
}

export function useAdminJobs(status: AgentJobStatus | undefined) {
  return useQuery({
    queryKey: ["admin", "jobs", status ?? "all"],
    queryFn: async () => (await fetchJobs({ status }, { limit: 50 }))!.data,
    retry: noRetryOnClientError,
  });
}

export function useJobRuns(id: string | null) {
  return useQuery({ queryKey: ["admin", "job-runs", id], enabled: id !== null, queryFn: () => fetchJobRuns(id!), retry: noRetryOnClientError });
}

export function useWorkspaceSettings(id: string | null) {
  return useQuery({ queryKey: ["admin", "workspace", id], enabled: id !== null, queryFn: () => fetchWorkspaceSettings(id!), retry: noRetryOnClientError });
}

export function useUpdateSettings(id: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (patch: AgentSettingsPatch) => updateWorkspaceSettings(id, patch),
    onSuccess: (ws) => qc.setQueryData(["admin", "workspace", id], ws),
  });
}

export function useBootstrapRules(id: string) {
  return useMutation({ mutationFn: () => bootstrapRules(id) });
}
