import { useState } from "react";
import type { AgentJob, AgentJobStatus } from "../../api/agents";
import { agentLabel, formatTime, outcomeLabel } from "../../api/agentText";
import ErrorMessage from "../../components/ErrorMessage";
import { useAdminJobs, useJobRuns } from "../../hooks/useAdmin";

const STATUSES: AgentJobStatus[] = ["pending", "running", "awaiting_approval", "completed", "failed", "dismissed"];

export default function JobsPanel() {
  const [status, setStatus] = useState<AgentJobStatus | "">("");
  const [open, setOpen] = useState<string | null>(null);
  const list = useAdminJobs(status || undefined);
  const jobs = list.data?.agent_jobs ?? [];

  return (
    <section aria-label="Agent jobs">
      <h2>Agent jobs</h2>
      <div className="filters">
        <label>
          Status
          <select value={status} onChange={(e) => setStatus(e.target.value as AgentJobStatus | "")}>
            <option value="">Any</option>
            {STATUSES.map((s) => (
              <option key={s} value={s}>
                {s.replace(/_/g, " ")}
              </option>
            ))}
          </select>
        </label>
      </div>
      {list.isPending && <p role="status">Loading jobs...</p>}
      {list.isError && <ErrorMessage error={list.error} onRetry={() => void list.refetch()} />}
      {list.data && jobs.length === 0 && <p>No jobs match.</p>}
      {jobs.length > 0 && (
        <table className="sidebyside">
          <thead>
            <tr>
              <th scope="col">Agent</th>
              <th scope="col">Status</th>
              <th scope="col">Attempts</th>
              <th scope="col">Updated</th>
              <th scope="col">History</th>
            </tr>
          </thead>
          <tbody>
            {jobs.map((j) => (
              <JobRows key={j.id} job={j} open={open === j.id} onToggle={() => setOpen(open === j.id ? null : j.id)} />
            ))}
          </tbody>
        </table>
      )}
      {list.data?.next_cursor && <p className="meta">Showing the newest 50 jobs. Filter by status to narrow the list.</p>}
    </section>
  );
}

function JobRows({ job, open, onToggle }: { job: AgentJob; open: boolean; onToggle: () => void }) {
  return (
    <>
      <tr>
        <th scope="row">{agentLabel(job.agent_type)}</th>
        <td>{job.status.replace(/_/g, " ")}</td>
        <td>{job.attempts}</td>
        <td>{formatTime(job.updated_at)}</td>
        <td>
          <button type="button" aria-expanded={open} aria-label={`${open ? "Hide" : "Show"} runs of ${agentLabel(job.agent_type)} ${job.id}`} onClick={onToggle}>
            {open ? "Hide runs" : "Show runs"}
          </button>
        </td>
      </tr>
      {open && (
        <tr>
          <td colSpan={5}>
            <Runs id={job.id} />
          </td>
        </tr>
      )}
    </>
  );
}

function Runs({ id }: { id: string }) {
  const q = useJobRuns(id);
  if (q.isPending) return <p role="status">Loading runs...</p>;
  if (q.isError) return <ErrorMessage error={q.error} onRetry={() => void q.refetch()} />;
  const { job, runs } = q.data;
  const prompt = runs.reduce((n, r) => n + r.prompt_tokens, 0);
  const completion = runs.reduce((n, r) => n + r.completion_tokens, 0);
  return (
    <div aria-label={`Runs of job ${id}`}>
      <p>
        {runs.length} {runs.length === 1 ? "turn" : "turns"}, {prompt} prompt tokens and {completion} completion tokens ({prompt + completion} in all).
      </p>
      {job.error_message && <p className="error">{job.error_message}</p>}
      {runs.length > 0 && (
        <table className="sidebyside">
          <thead>
            <tr>
              <th scope="col">Turn</th>
              <th scope="col">Model</th>
              <th scope="col">Tool</th>
              <th scope="col">Outcome</th>
              <th scope="col">Tokens in</th>
              <th scope="col">Tokens out</th>
              <th scope="col">Latency</th>
            </tr>
          </thead>
          <tbody>
            {runs.map((r) => (
              <tr key={r.turn}>
                <th scope="row">{r.turn}</th>
                <td>{r.model}</td>
                <td>{r.tool_name ?? "-"}</td>
                <td>{outcomeLabel(r.outcome)}</td>
                <td>{r.prompt_tokens}</td>
                <td>{r.completion_tokens}</td>
                <td>{r.latency_ms} ms</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
