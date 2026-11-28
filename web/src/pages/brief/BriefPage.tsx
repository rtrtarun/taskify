import { Link } from "react-router-dom";
import type { MorningBrief } from "../../api/agents";
import { agentLabel, formatTime, toolLabel } from "../../api/agentText";
import { ApiError } from "../../api/client";
import { PRIORITY_LABEL, STATUS_LABEL, type Priority, type TaskStatus } from "../../api/tasks";
import ErrorMessage from "../../components/ErrorMessage";
import { useBrief } from "../../hooks/useAgents";

type BriefTask = MorningBrief["top_items"][number];

function TaskItems({ items, total, label }: { items: BriefTask[]; total: number; label: string }) {
  return (
    <>
      <ul aria-label={label}>
        {items.map((t) => (
          <li key={t.task_id}>
            <Link to={`/tasks/${t.task_id}`}>{t.title}</Link> - {STATUS_LABEL[t.status as TaskStatus] ?? t.status},{" "}
            {PRIORITY_LABEL[t.priority as Priority] ?? t.priority}
            {t.due_at ? `, due ${formatTime(t.due_at)}` : ""}
            {t.overdue ? " (overdue)" : ""}
          </li>
        ))}
      </ul>
      {total > items.length && <p className="meta">and {total - items.length} more</p>}
    </>
  );
}

/** The caller's latest morning brief, the page its notification's view link opens. */
export default function BriefPage() {
  const brief = useBrief();
  const missing = brief.error instanceof ApiError && brief.error.status === 404;

  return (
    <main>
      <h1>Morning brief</h1>
      {brief.isPending && <p role="status">Loading your brief...</p>}
      {missing && <p>You have no brief yet. A brief is written each morning when there is something to report.</p>}
      {brief.isError && !missing && <ErrorMessage error={brief.error} onRetry={() => void brief.refetch()} />}
      {brief.data && <Brief brief={brief.data} />}
    </main>
  );
}

function Brief({ brief }: { brief: MorningBrief }) {
  return (
    <>
      <p className="meta">
        For <time dateTime={brief.brief_date}>{brief.brief_date}</time>, written{" "}
        <time dateTime={brief.written_at}>{formatTime(brief.written_at)}</time>. You have {brief.open_tasks} open{" "}
        {brief.open_tasks === 1 ? "task" : "tasks"}.
      </p>
      {brief.text && <p className="brief-text">{brief.text}</p>}

      <section aria-labelledby="top-h">
        <h2 id="top-h">Top of your list</h2>
        {brief.top_items.length === 0 ? <p>Nothing stands out.</p> : <TaskItems items={brief.top_items} total={brief.top_items.length} label="Top items" />}
      </section>

      <section aria-labelledby="approvals-h">
        <h2 id="approvals-h">Waiting for your approval</h2>
        {brief.pending_approvals.length === 0 ? (
          <p>Nothing is waiting for you.</p>
        ) : (
          <>
            <ul aria-label="Pending approvals">
              {brief.pending_approvals.map((a) => (
                <li key={a.job_id}>
                  {agentLabel(a.agent_type)} proposes {a.proposed_tools.map(toolLabel).join(", ")} on <Link to={`/tasks/${a.task_id}`}>{a.task_title}</Link>
                </li>
              ))}
            </ul>
            {brief.pending_approvals_total > brief.pending_approvals.length && (
              <p className="meta">and {brief.pending_approvals_total - brief.pending_approvals.length} more</p>
            )}
            <p>
              <Link to="/approvals">Open the approvals inbox</Link>
            </p>
          </>
        )}
      </section>

      <section aria-labelledby="unblocked-h">
        <h2 id="unblocked-h">Newly unblocked</h2>
        {brief.unblocked_tasks.length === 0 ? (
          <p>Nothing was unblocked.</p>
        ) : (
          <TaskItems items={brief.unblocked_tasks} total={brief.unblocked_total} label="Unblocked tasks" />
        )}
      </section>
    </>
  );
}
