import { PRIORITY_LABEL, STATUS_LABEL, type Priority, type TaskDetail, type TaskStatus } from "./tasks";
import type { AgentType } from "./agents";

/** How each agent is named on screen. */
export const AGENT_LABEL: Record<AgentType, string> = {
  triage: "Triage agent",
  decomposer: "Decomposer",
  blocker_investigator: "Blocker investigator",
  digest: "Digest agent",
  learning: "Learning agent",
};

export function agentLabel(type: string): string {
  return (AGENT_LABEL as Record<string, string>)[type] ?? type;
}

const TOOL_LABEL: Record<string, string> = {
  ArchiveTask: "Archive the task",
  CreateSubtaskDAG: "Create subtasks",
  DraftResolutionComment: "Post a comment",
  LinkDependencies: "Add dependencies",
  ProposeNotificationRule: "Change a notification rule",
  ReassignTask: "Reassign the task",
  ShiftDueDate: "Move the due date",
  UpdateTaskAttributes: "Update task details",
};

export const toolLabel = (tool: string): string => TOOL_LABEL[tool] ?? tool;

/** What a run turn's outcome means to a person (see internal/agent/runtime.go). */
const OUTCOME_LABEL: Record<string, string> = {
  tool_call: "Done",
  proposal: "Proposed, waiting for a decision",
  rejected: "Refused: the request was not valid",
  rule_rejected: "Refused by a rule",
  version_conflict: "Not applied: the task changed meanwhile",
  tool_error: "Failed",
  llm_error: "The model could not be reached",
  final: "Finished",
};

export const outcomeLabel = (outcome: string): string => OUTCOME_LABEL[outcome] ?? outcome;

/** Text for any JSON value. It is always rendered as text. */
export function formatValue(v: unknown): string {
  if (v === null || v === undefined) return "-";
  if (typeof v === "string") return v === "" ? "(empty)" : v;
  if (Array.isArray(v)) return v.length === 0 ? "(none)" : v.map(formatValue).join(", ");
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);

/** The model's reasoning in a call's arguments, or in a proposal's. */
export function reasoningOf(args: unknown): string | null {
  if (!isObject(args)) return null;
  const r = args.reasoning ?? args.rationale;
  return typeof r === "string" && r.trim() !== "" ? r : null;
}

/** One line of a change: a field and the value the agent gave it. */
export interface Change {
  field: string;
  value: string;
}

const HIDDEN_ARGS = new Set(["reasoning", "rationale", "task_id", "parent_task_id"]);

const FIELD_LABEL: Record<string, string> = {
  priority: "Priority",
  labels: "Labels",
  description: "Description",
  estimated_minutes: "Estimate (minutes)",
  assignee_id: "Assignee",
  due_at: "Due",
  body: "Comment",
  depends_on_task_ids: "Waits for",
  subtasks: "Subtasks",
  pattern_id: "Pattern",
  proposed_adjustment: "Score adjustment",
};

const fieldLabel = (k: string): string => FIELD_LABEL[k] ?? k;

function subtasksText(v: unknown): string {
  if (!Array.isArray(v)) return formatValue(v);
  return v
    .map((s) => {
      if (!isObject(s)) return formatValue(s);
      const after = Array.isArray(s.depends_on) && s.depends_on.length > 0 ? ` (after ${s.depends_on.map(formatValue).join(", ")})` : "";
      return `${formatValue(s.title)}${after}`;
    })
    .join("; ");
}

function fieldValue(k: string, v: unknown): string {
  if (k === "subtasks") return subtasksText(v);
  if (k === "priority" && typeof v === "string") return PRIORITY_LABEL[v as Priority] ?? v;
  if (k === "due_at" && typeof v === "string") return formatTime(v);
  return formatValue(v);
}

export function formatTime(iso: string | null | undefined): string {
  if (!iso) return "None";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toISOString().replace(/:\d\d\.\d{3}Z$/, "Z").replace("T", " ");
}

/** The fields a tool call changes, minus the ones that only identify or explain it. */
export function changesOf(args: unknown): Change[] {
  if (!isObject(args)) return args === undefined || args === null ? [] : [{ field: "Arguments", value: formatValue(args) }];
  return Object.entries(args)
    .filter(([k]) => !HIDDEN_ARGS.has(k))
    .map(([k, v]) => ({ field: fieldLabel(k), value: fieldValue(k, v) }));
}

/** A proposal row: what the task has now beside what the agent wants. */
export interface ProposalRow {
  field: string;
  current: string;
  proposed: string;
}

const NOT_SHOWN = "(not shown here)";

/**
 * proposalRows lines a proposal up against the task as it is now. The
 * current value is read from the task the page loaded; where the task
 * response does not carry it, the column says so rather than guess.
 */
export function proposalRows(tool: string, args: Record<string, unknown>, task: TaskDetail | undefined): ProposalRow[] {
  const cur = (get: (t: TaskDetail) => string): string => (task ? get(task) : "...");
  const rows: ProposalRow[] = [];
  const add = (field: string, current: string, proposed: string) => rows.push({ field, current, proposed });
  switch (tool) {
    case "UpdateTaskAttributes":
      if ("priority" in args) add("Priority", cur((t) => PRIORITY_LABEL[t.priority]), fieldValue("priority", args.priority));
      if ("labels" in args) add("Labels", cur((t) => formatValue(t.labels)), formatValue(args.labels));
      if ("description" in args) add("Description", cur((t) => formatValue(t.description)), formatValue(args.description));
      if ("estimated_minutes" in args) add("Estimate (minutes)", NOT_SHOWN, formatValue(args.estimated_minutes));
      return rows;
    case "ReassignTask":
      add("Assignee", cur((t) => t.assignee_id ?? "Unassigned"), formatValue(args.assignee_id));
      return rows;
    case "ShiftDueDate":
      add("Due", cur((t) => formatTime(t.due_at)), fieldValue("due_at", args.due_at));
      return rows;
    case "ArchiveTask":
      add("Status", cur((t) => STATUS_LABEL[t.status as TaskStatus] ?? t.status), "Archived");
      return rows;
    case "DraftResolutionComment":
      add("New comment", "-", formatValue(args.body));
      return rows;
    default:
      for (const c of changesOf(args)) add(c.field, NOT_SHOWN, c.value);
      return rows;
  }
}

/** A tool's result as a short text, when it holds a message; otherwise null. */
export function resultText(result: unknown): string | null {
  if (typeof result === "string") return result;
  if (isObject(result)) {
    for (const k of ["error", "message", "reason"]) if (typeof result[k] === "string") return result[k] as string;
  }
  return null;
}
