import { Link } from "react-router-dom";
import type { AgentJobRuns, AgentRun } from "../../api/agents";
import { agentLabel, changesOf, outcomeLabel, reasoningOf, resultText, toolLabel, type Change } from "../../api/agentText";
import ErrorMessage from "../../components/ErrorMessage";
import { useAgentRuns } from "../../hooks/useAgents";

/** One tool call of a turn: what it was, what it changes, and why. */
interface Call {
  tool: string;
  args: unknown;
  result: unknown;
}

/** A turn with one call has the call's own arguments; one with several has a list of them. */
function callsOf(run: AgentRun): Call[] {
  if (run.tool_name === null) return [];
  if (run.tool_name !== "multiple") return [{ tool: run.tool_name, args: run.tool_args, result: run.tool_result }];
  const calls = Array.isArray(run.tool_args) ? (run.tool_args as { name?: string; arguments?: unknown }[]) : [];
  const results = Array.isArray(run.tool_result) ? (run.tool_result as unknown[]) : [];
  return calls.map((c, i) => ({ tool: c.name ?? "tool", args: c.arguments, result: results[i] }));
}

function ChangeList({ changes }: { changes: Change[] }) {
  if (changes.length === 0) return null;
  return (
    <ul className="changes">
      {changes.map((c) => (
        <li key={c.field}>
          {c.field}: {c.value}
        </li>
      ))}
    </ul>
  );
}

function Turn({ run }: { run: AgentRun }) {
  const calls = callsOf(run);
  if (calls.length === 0) return null;
  return (
    <div className="turn">
      {calls.map((c, i) => {
        const reasoning = reasoningOf(c.args);
        const failure = run.outcome === "rule_rejected" || run.outcome === "tool_error" || run.outcome === "rejected" ? resultText(c.result) : null;
        return (
          <div key={i}>
            <p>
              <strong>{toolLabel(c.tool)}</strong> - {outcomeLabel(run.outcome)}
            </p>
            <ChangeList changes={changesOf(c.args)} />
            {failure && <p>{failure}</p>}
            {reasoning && <blockquote aria-label="Reasoning">{reasoning}</blockquote>}
          </div>
        );
      })}
    </div>
  );
}

function JobEntry({ entry }: { entry: AgentJobRuns }) {
  const { job, runs } = entry;
  const turns = runs.filter((r) => r.tool_name !== null);
  return (
    <li>
      <h3>
        {agentLabel(job.agent_type)} <span className="meta">({job.status.replace("_", " ")})</span>{" "}
        <time dateTime={job.created_at} className="meta">
          {new Date(job.created_at).toLocaleString()}
        </time>
      </h3>
      {job.status === "awaiting_approval" && (
        <p>
          Waiting for a decision. <Link to="/approvals">Open the approvals inbox</Link>.
        </p>
      )}
      {job.status === "failed" && job.error_message && <p>It failed: {job.error_message}</p>}
      {turns.length === 0 && job.status !== "awaiting_approval" && <p className="meta">No changes proposed or made.</p>}
      {turns.map((r) => (
        <Turn key={r.turn} run={r} />
      ))}
    </li>
  );
}

/** What agents did on this task, oldest first, naming the agent and its reasoning. */
export default function AgentActivity({ taskId, pollMs }: { taskId: string; pollMs?: number }) {
  const runs = useAgentRuns(taskId, pollMs);
  return (
    <section aria-labelledby="activity-h">
      <h2 id="activity-h">Agent activity</h2>
      {runs.isPending && <p role="status">Loading agent activity...</p>}
      {runs.isError && <ErrorMessage error={runs.error} onRetry={() => void runs.refetch()} />}
      {runs.data && runs.data.length === 0 && <p>No agent has worked on this task.</p>}
      {runs.data && runs.data.length > 0 && (
        <ol className="timeline" aria-label="Agent activity">
          {runs.data.map((e) => (
            <JobEntry key={e.job.id} entry={e} />
          ))}
        </ol>
      )}
    </section>
  );
}
