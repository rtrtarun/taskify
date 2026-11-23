import { Link } from "react-router-dom";
import type { AgentJob, Subtask } from "../../api/agents";
import { STATUS_LABEL, type TaskStatus } from "../../api/tasks";
import DependencyGraph, { type GraphEdge, type GraphNode } from "../../components/DependencyGraph";
import ErrorMessage from "../../components/ErrorMessage";
import { useDecompose, useDecomposeJob, useSubtasks } from "../../hooks/useAgents";

/** What the decomposer is doing, in words. */
export function jobProgress(job: AgentJob): string {
  switch (job.status) {
    case "pending":
      return "Queued: the decomposer will start shortly.";
    case "running":
      return "The decomposer is working on a plan.";
    case "awaiting_approval":
      return "The decomposer made a plan and is waiting for a decision.";
    case "completed":
      return "The last decomposition is done.";
    case "failed":
      return "The last decomposition failed.";
    case "dismissed":
      return "The last plan was rejected.";
  }
}

export default function Subtasks({ taskId, taskStatus, pollMs, jobPollMs }: { taskId: string; taskStatus: string; pollMs?: number; jobPollMs?: number }) {
  const subtasks = useSubtasks(taskId, pollMs);
  const job = useDecomposeJob(taskId, jobPollMs);
  const decompose = useDecompose(taskId);

  const latest = job.data ?? null;
  const busy = latest?.status === "pending" || latest?.status === "running" || latest?.status === "awaiting_approval" || decompose.isPending;
  const finished = taskStatus === "completed" || taskStatus === "archived";

  return (
    <section aria-labelledby="subtasks-h" className="subtasks">
      <h2 id="subtasks-h">Subtasks</h2>

      <div className="actions" role="group" aria-label="Decompose">
        <button type="button" onClick={() => decompose.mutate()} disabled={busy || finished}>
          Decompose
        </button>
      </div>
      {decompose.isError && <ErrorMessage error={decompose.error} />}
      {job.isError && <ErrorMessage error={job.error} onRetry={() => void job.refetch()} />}
      {latest && (
        <p role="status" aria-label="Decomposition progress">
          {jobProgress(latest)}
          {latest.status === "failed" && latest.error_message ? ` ${latest.error_message}` : ""}
          {latest.status === "awaiting_approval" && (
            <>
              {" "}
              <Link to="/approvals">Review it in the approvals inbox</Link>.
            </>
          )}
        </p>
      )}

      {subtasks.isPending && <p role="status">Loading subtasks...</p>}
      {subtasks.isError && <ErrorMessage error={subtasks.error} onRetry={() => void subtasks.refetch()} />}
      {subtasks.data && <SubtaskView subtasks={subtasks.data} />}
    </section>
  );
}

/** The children of a task: a graph when they depend on each other, and always a list. */
export function SubtaskView({ subtasks }: { subtasks: Subtask[] }) {
  if (subtasks.length === 0) return <p>No subtasks yet.</p>;

  const byId = new Map(subtasks.map((s) => [s.task.id, s.task]));
  const nodes: GraphNode[] = subtasks.map((s) => ({ id: s.task.id, title: s.task.title, status: s.task.status }));
  const edges: GraphEdge[] = subtasks.flatMap((s) => s.depends_on.filter((d) => byId.has(d)).map((d) => ({ from: d, to: s.task.id })));

  return (
    <>
      {edges.length > 0 && (
        <div className="graph-wrap">
          <DependencyGraph nodes={nodes} edges={edges} />
        </div>
      )}
      <ul aria-label="Subtasks">
        {subtasks.map((s) => {
          const inside = s.depends_on.filter((d) => byId.has(d));
          const outside = s.depends_on.length - inside.length;
          return (
            <li key={s.task.id}>
              <Link to={`/tasks/${s.task.id}`}>{s.task.title}</Link> - {STATUS_LABEL[s.task.status as TaskStatus] ?? s.task.status}
              {inside.length > 0 && <> - waits for {inside.map((d) => byId.get(d)!.title).join(", ")}</>}
              {outside > 0 && <> - and {outside} other {outside === 1 ? "task" : "tasks"}</>}
            </li>
          );
        })}
      </ul>
    </>
  );
}
