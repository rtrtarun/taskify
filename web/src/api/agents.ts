import { api, unwrap, type Schemas } from "./client";

export type Subtask = Schemas["Subtask"];
export type AgentJob = Schemas["AgentJob"];
export type AgentJobList = Schemas["AgentJobList"];
export type AgentJobStatus = Schemas["AgentJobStatus"];
export type AgentType = Schemas["AgentType"];
export type AgentJobRuns = Schemas["AgentJobRuns"];
export type AgentRun = Schemas["AgentRun"];
export type Proposal = Schemas["Proposal"];
export type MorningBrief = Schemas["MorningBrief"];

export async function fetchSubtasks(taskId: string): Promise<Subtask[]> {
  const body = unwrap(await api.GET("/api/v1/tasks/{id}/subtasks", { params: { path: { id: taskId } }, cache: "no-store" }));
  return body.subtasks;
}

export async function fetchAgentRuns(taskId: string): Promise<AgentJobRuns[]> {
  const body = unwrap(await api.GET("/api/v1/tasks/{id}/agent-runs", { params: { path: { id: taskId } }, cache: "no-store" }));
  return body.agent_runs;
}

export interface JobFilters {
  status?: AgentJobStatus;
  task_id?: string;
  agent_type?: AgentType;
}

/** A page of jobs with the ETag that names it, kept to poll cheaply. */
export interface JobsSnapshot {
  data: AgentJobList;
  etag: string | null;
}

/** fetchJobs reads one page; with the last ETag a 304 returns null. */
export async function fetchJobs(filters: JobFilters, opts: { etag?: string | null; limit?: number } = {}): Promise<JobsSnapshot | null> {
  const result = await api.GET("/api/v1/agent-jobs", {
    params: { query: { ...filters, limit: opts.limit ?? 50 }, header: opts.etag ? { "If-None-Match": opts.etag } : {} },
    cache: "no-store",
  });
  if (result.response.status === 304) return null;
  return { data: unwrap(result), etag: result.response.headers.get("ETag") };
}

/** decomposeTask queues a decomposition, or returns the one already open. */
export async function decomposeTask(taskId: string): Promise<AgentJob> {
  return unwrap(await api.POST("/api/v1/tasks/{id}/decompose", { params: { path: { id: taskId } } }));
}

export async function applyJob(id: string): Promise<Schemas["AgentJobDecision"]> {
  return unwrap(await api.POST("/api/v1/agent-jobs/{id}/apply", { params: { path: { id } } }));
}

export async function dismissJob(id: string): Promise<Schemas["AgentJobDecision"]> {
  return unwrap(await api.POST("/api/v1/agent-jobs/{id}/dismiss", { params: { path: { id } } }));
}

export async function fetchMyBrief(): Promise<MorningBrief> {
  return unwrap(await api.GET("/api/v1/me/brief", { cache: "no-store" }));
}
