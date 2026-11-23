import { api, unwrap, type Schemas } from "./client";

export type Suggestion = Schemas["LearningSuggestion"];
export type OutboxEntry = Schemas["OutboxEntry"];
export type OutboxStatus = Schemas["OutboxStatus"];
export type FullWorkspace = Schemas["Workspace"];
export type AgentSettings = Schemas["AgentSettings"];
export type AgentSettingsPatch = Schemas["AgentSettingsPatch"];
export type Rule = Schemas["Rule"];

export async function fetchSuggestions(): Promise<Suggestion[]> {
  return unwrap(await api.GET("/api/v1/admin/learning/suggestions", { cache: "no-store" })).suggestions;
}

export async function decideSuggestion(id: string, decision: "approve" | "reject"): Promise<Suggestion> {
  return unwrap(await api.POST("/api/v1/admin/learning/suggestions/{id}/decision", { params: { path: { id } }, body: { decision } }));
}

/** fetchOutbox reads one page of the tenant's outbox rows, newest first. */
export async function fetchOutbox(status: OutboxStatus | undefined, cursor?: string): Promise<{ entries: OutboxEntry[]; next: string | null }> {
  const body = unwrap(await api.GET("/api/v1/admin/outbox", { params: { query: { status, cursor, limit: 50 } }, cache: "no-store" }));
  return { entries: body.entries, next: body.next_cursor };
}

export async function replayOutbox(id: string): Promise<OutboxEntry> {
  return unwrap(await api.POST("/api/v1/admin/outbox/{id}/replay", { params: { path: { id } } }));
}

export async function fetchJobRuns(id: string): Promise<Schemas["AgentJobRuns"]> {
  return unwrap(await api.GET("/api/v1/admin/agent-jobs/{id}/runs", { params: { path: { id } }, cache: "no-store" }));
}

export async function fetchWorkspaceSettings(id: string): Promise<FullWorkspace> {
  return unwrap(await api.GET("/api/v1/workspaces/{id}/settings", { params: { path: { id } }, cache: "no-store" }));
}

export async function updateWorkspaceSettings(id: string, agent_settings: AgentSettingsPatch): Promise<FullWorkspace> {
  return unwrap(await api.PATCH("/api/v1/workspaces/{id}/settings", { params: { path: { id } }, body: { agent_settings } }));
}

export async function bootstrapRules(id: string): Promise<Rule[]> {
  return unwrap(await api.POST("/api/v1/workspaces/{id}/bootstrap-rules", { params: { path: { id } } })).rules;
}
