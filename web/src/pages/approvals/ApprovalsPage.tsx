import { useState } from "react";
import { Link } from "react-router-dom";
import type { AgentJob } from "../../api/agents";
import { agentLabel, formatTime, proposalRows, reasoningOf, toolLabel } from "../../api/agentText";
import { ApiError } from "../../api/client";
import ErrorMessage, { describeError } from "../../components/ErrorMessage";
import { useAwaitingApprovals, useDecideJob, useTaskNow, type Decision } from "../../hooks/useAgents";

/** What a person is told when a decision does not go through. The server's code decides; its text is not parsed. */
export function describeDecisionError(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case "stale_version":
        return "This proposal is out of date: the task changed after the agent made it, so nothing was applied and the proposal is closed. The agent can propose again.";
      case "already_decided":
        return "Someone else already decided this proposal. Nothing was changed.";
      case "not_awaiting_approval":
        return "This proposal is no longer waiting for a decision.";
      case "forbidden":
        return "Only the task's assignee, its creator, or an admin can decide this proposal.";
    }
  }
  return describeError(err);
}

/** A decision the person just made, kept on screen after the job leaves the inbox. */
interface Outcome {
  id: string;
  ok: boolean;
  text: string;
}

export default function ApprovalsPage({ pollMs }: { pollMs?: number }) {
  const inbox = useAwaitingApprovals(pollMs);
  const decide = useDecideJob();
  const [outcomes, setOutcomes] = useState<Outcome[]>([]);
  const [busy, setBusy] = useState<string | null>(null);

  const jobs = inbox.data?.data.agent_jobs ?? [];
  const more = inbox.data?.data.next_cursor != null;

  function act(job: AgentJob, title: string, decision: Decision) {
    setBusy(job.id);
    const subject = `${agentLabel(job.agent_type)}'s proposal for "${title}"`;
    decide.mutate(
      { id: job.id, decision },
      {
        onSuccess: (res) =>
          setOutcomes((o) => [
            { id: job.id, ok: true, text: res.changed ? `${decision === "approve" ? "Approved and applied" : "Rejected"}: ${subject}.` : `Already decided: ${subject}. Nothing changed.` },
            ...o.filter((x) => x.id !== job.id),
          ]),
        onError: (err) => setOutcomes((o) => [{ id: job.id, ok: false, text: `${subject}: ${describeDecisionError(err)}` }, ...o.filter((x) => x.id !== job.id)]),
        onSettled: () => setBusy(null),
      },
    );
  }

  return (
    <main>
      <h1>Approvals</h1>
      <p className="meta">Changes the agents propose wait here until you approve or reject them. Nothing is applied before then.</p>

      {outcomes.length > 0 && (
        <ul aria-label="Decisions" className="outcomes">
          {outcomes.map((o) => (
            <li key={o.id} role={o.ok ? "status" : "alert"} className={o.ok ? undefined : "error"}>
              {o.text}
            </li>
          ))}
        </ul>
      )}

      {inbox.isPending && <p role="status">Loading proposals...</p>}
      {inbox.isError && !inbox.data && <ErrorMessage error={inbox.error} onRetry={() => void inbox.refetch()} />}
      {inbox.isError && inbox.data && <p role="status">Could not refresh: {describeError(inbox.error)} Showing the last list.</p>}

      {inbox.data && jobs.length === 0 && <p>Nothing is waiting for your decision.</p>}
      {jobs.map((job) => (
        <ProposalCard key={job.id} job={job} busy={busy === job.id} onDecide={(title, d) => act(job, title, d)} />
      ))}
      {more && <p className="meta">More proposals are waiting. Decide some of these to see them.</p>}
    </main>
  );
}

function ProposalCard({ job, busy, onDecide }: { job: AgentJob; busy: boolean; onDecide: (title: string, d: Decision) => void }) {
  const task = useTaskNow(job.task_id);
  const title = task.data?.title ?? (job.task_id ? "a task" : "your workspace");
  const stale = task.data && job.base_task_version !== null && task.data.version > job.base_task_version;
  const proposals = job.proposed_actions ?? [];

  return (
    <article className="card proposal" aria-label={`Proposal from ${agentLabel(job.agent_type)}`}>
      <h2>
        {agentLabel(job.agent_type)} proposes changes to{" "}
        {job.task_id ? <Link to={`/tasks/${job.task_id}`}>{task.data?.title ?? "this task"}</Link> : "something outside a task"}
      </h2>
      <p className="meta">
        Asked <time dateTime={job.updated_at}>{formatTime(job.updated_at)}</time>
        {job.base_task_version !== null && ` - based on version ${job.base_task_version}`}
      </p>

      {stale && (
        <p role="alert" className="stale">
          This proposal is out of date: the task has changed since the agent looked at it (it was version {job.base_task_version}, it is now
          version {task.data!.version}). Approving it will not apply it.
        </p>
      )}
      {task.isError && <ErrorMessage error={task.error} />}

      {proposals.map((p, i) => {
        const reasoning = reasoningOf(p.arguments);
        const rows = proposalRows(p.tool, p.arguments, task.data);
        return (
          <section key={p.call_id ?? i} aria-label={toolLabel(p.tool)}>
            <h3>{toolLabel(p.tool)}</h3>
            {rows.length > 0 && (
              <table className="sidebyside">
                <thead>
                  <tr>
                    <th scope="col">Field</th>
                    <th scope="col">Current</th>
                    <th scope="col">Proposed</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r) => (
                    <tr key={r.field}>
                      <th scope="row">{r.field}</th>
                      <td>{r.current}</td>
                      <td className="proposed">{r.proposed}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {reasoning && <blockquote aria-label="Reasoning">{reasoning}</blockquote>}
          </section>
        );
      })}
      {proposals.length === 0 && <p>This proposal lists no changes.</p>}

      <div className="actions" role="group" aria-label="Decide">
        <button type="button" onClick={() => onDecide(title, "approve")} disabled={busy}>
          Approve
        </button>
        <button type="button" onClick={() => onDecide(title, "reject")} disabled={busy}>
          Reject
        </button>
      </div>
    </article>
  );
}
