import { useState, type FormEvent } from "react";
import { Link, useParams } from "react-router-dom";
import { ApiError, type Schemas } from "../../api/client";
import { PRIORITIES, PRIORITY_LABEL, STATUSES, STATUS_LABEL, type Priority, type TaskDetail, type TaskStatus } from "../../api/tasks";
import ErrorMessage from "../../components/ErrorMessage";
import {
  useAddComment,
  useArchiveTask,
  useCompleteTask,
  useRestoreTask,
  useTaskDetail,
  useUpdateTask,
} from "../../hooks/useTaskDetail";

/** The text fields the user edits together and saves with one button. */
interface Draft {
  title: string;
  description: string;
  assignee: string;
  labels: string;
  watchers: string;
  due: string; // value of a datetime-local input
}

const list = (s: string): string[] => s.split(",").map((x) => x.trim()).filter(Boolean);
const toLocal = (iso: string | null): string => (iso ? new Date(iso).toISOString().slice(0, 16) : "");

function draftFrom(t: TaskDetail): Draft {
  return {
    title: t.title,
    description: t.description,
    assignee: t.assignee_id ?? "",
    labels: t.labels.join(", "),
    watchers: t.watcher_ids.join(", "),
    due: toLocal(t.due_at),
  };
}

/** The fields of the draft that differ from the task, in the shape the API takes. */
function changesFrom(t: TaskDetail, d: Draft): Schemas["TaskUpdate"] {
  const c: Schemas["TaskUpdate"] = {};
  if (d.title.trim() !== t.title) c.title = d.title.trim();
  if (d.description !== t.description) c.description = d.description;
  if (d.assignee.trim() !== (t.assignee_id ?? "")) c.assignee_id = d.assignee.trim() || null;
  if (JSON.stringify(list(d.labels)) !== JSON.stringify(t.labels)) c.labels = list(d.labels);
  if (JSON.stringify(list(d.watchers)) !== JSON.stringify(t.watcher_ids)) c.watcher_ids = list(d.watchers);
  if (d.due !== toLocal(t.due_at)) c.due_at = d.due ? new Date(d.due + "Z").toISOString() : null;
  return c;
}

export default function TaskDetailPage() {
  const { id = "" } = useParams();
  const detail = useTaskDetail(id);

  if (detail.isPending) {
    return (
      <main>
        <p role="status">Loading task...</p>
      </main>
    );
  }
  if (detail.isError) {
    const gone = detail.error instanceof ApiError && detail.error.status === 404;
    return (
      <main>
        <h1>Task</h1>
        {gone ? (
          <p role="alert">This task does not exist, or you cannot see it.</p>
        ) : (
          <ErrorMessage error={detail.error} onRetry={() => void detail.refetch()} />
        )}
        <Link to="/tasks">Back to tasks</Link>
      </main>
    );
  }
  return <Loaded task={detail.data} />;
}

function Loaded({ task }: { task: TaskDetail }) {
  const update = useUpdateTask(task.id);
  const complete = useCompleteTask(task.id);
  const archive = useArchiveTask(task.id);
  const restore = useRestoreTask(task.id);
  const comment = useAddComment(task.id);

  const [draft, setDraft] = useState<Draft | null>(null);
  // What the user tried to save when the server said the task had changed.
  const [lost, setLost] = useState<string[] | null>(null);
  const [commentText, setCommentText] = useState("");

  const shown = draft ?? draftFrom(task);
  const set = (field: keyof Draft, value: string) => setDraft({ ...shown, [field]: value });
  const changes = draft ? changesFrom(task, draft) : {};
  const dirty = Object.keys(changes).length > 0;

  function save(vars: Schemas["TaskUpdate"], after?: () => void) {
    update.mutate(
      { changes: vars, version: task.version },
      {
        onSuccess: () => {
          setLost(null);
          after?.();
        },
        onError: (err) => {
          if (err instanceof ApiError && err.code === "version_conflict") {
            // Nothing is dropped silently: the edit is listed in the notice.
            setLost(Object.entries(vars).map(([k, v]) => `${k}: ${Array.isArray(v) ? v.join(", ") : String(v ?? "none")}`));
            setDraft(null);
          }
        },
      },
    );
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    if (dirty) save(changes, () => setDraft(null));
  }

  function act(m: typeof complete) {
    m.mutate(task.version, { onSuccess: () => setLost(null) });
  }

  const conflict = (m: { error: unknown }) => m.error instanceof ApiError && m.error.code === "version_conflict";
  const actionError = [complete, archive, restore].find((m) => m.isError && !conflict(m));
  const actionConflict = [complete, archive, restore].some((m) => m.isError && conflict(m));
  const updateConflict = update.isError && conflict(update);
  const open = task.status !== "completed" && task.status !== "archived";

  return (
    <main className="detail">
      <p>
        <Link to="/tasks">Back to tasks</Link>
      </p>
      <h1>{task.title}</h1>
      <p className="meta">
        {STATUS_LABEL[task.status]} - version {task.version} - created by {task.creator_id} in {task.workspace_id}
        {task.blocked_reason ? ` - blocked: ${task.blocked_reason}` : ""}
      </p>

      {(updateConflict || actionConflict) && (
        <div role="alert" className="conflict">
          <p>Someone else changed this task first, so your change was not saved. The latest version is shown below.</p>
          {lost && lost.length > 0 && (
            <>
              <p>What you tried to save:</p>
              <ul>
                {lost.map((l) => (
                  <li key={l}>{l}</li>
                ))}
              </ul>
            </>
          )}
        </div>
      )}
      {update.isError && !updateConflict && <ErrorMessage error={update.error} />}
      {actionError && <ErrorMessage error={actionError.error} />}

      <div className="actions" role="group" aria-label="Task actions">
        {open && (
          <button type="button" onClick={() => act(complete)} disabled={complete.isPending}>
            Complete
          </button>
        )}
        {task.status !== "archived" ? (
          <button type="button" onClick={() => act(archive)} disabled={archive.isPending}>
            Archive
          </button>
        ) : (
          <button type="button" onClick={() => act(restore)} disabled={restore.isPending}>
            Restore
          </button>
        )}
        <label>
          Status
          <select
            value={task.status}
            onChange={(e) => save({ status: e.target.value as TaskStatus })}
            disabled={task.status === "archived"}
          >
            {STATUSES.filter((s) => s !== "archived").map((s) => (
              <option key={s} value={s}>
                {STATUS_LABEL[s]}
              </option>
            ))}
            {task.status === "archived" && <option value="archived">Archived</option>}
          </select>
        </label>
        <label>
          Priority
          <select value={task.priority} onChange={(e) => save({ priority: e.target.value as Priority })}>
            {PRIORITIES.map((p) => (
              <option key={p} value={p}>
                {PRIORITY_LABEL[p]}
              </option>
            ))}
          </select>
        </label>
      </div>

      <form onSubmit={submit} className="edit" aria-label="Edit task">
        <label>
          Title
          <input value={shown.title} onChange={(e) => set("title", e.target.value)} required maxLength={200} />
        </label>
        <label>
          Description
          <textarea value={shown.description} onChange={(e) => set("description", e.target.value)} rows={5} />
        </label>
        <label>
          Assignee
          <input value={shown.assignee} onChange={(e) => set("assignee", e.target.value)} placeholder="Unassigned" />
        </label>
        <label>
          Labels (comma separated)
          <input value={shown.labels} onChange={(e) => set("labels", e.target.value)} />
        </label>
        <label>
          Watchers (comma separated)
          <input value={shown.watchers} onChange={(e) => set("watchers", e.target.value)} />
        </label>
        <label>
          Due (UTC)
          <input type="datetime-local" value={shown.due} onChange={(e) => set("due", e.target.value)} />
        </label>
        <button type="submit" disabled={!dirty || update.isPending}>
          {update.isPending ? "Saving..." : "Save changes"}
        </button>
      </form>

      <section aria-labelledby="comments-h">
        <h2 id="comments-h">Comments</h2>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            comment.mutate(commentText.trim(), { onSuccess: () => setCommentText("") });
          }}
          aria-label="Add a comment"
        >
          <label>
            Comment
            <textarea value={commentText} onChange={(e) => setCommentText(e.target.value)} rows={3} required />
          </label>
          <button type="submit" disabled={comment.isPending || commentText.trim() === ""}>
            {comment.isPending ? "Posting..." : "Add comment"}
          </button>
        </form>
        {comment.isError && <ErrorMessage error={comment.error} />}
        {task.comments.length === 0 ? (
          <p>No comments yet.</p>
        ) : (
          <ul className="comments">
            {task.comments.map((c) => (
              <li key={c.id}>
                <strong>{c.author_id}</strong>{" "}
                <time dateTime={c.created_at}>{new Date(c.created_at).toLocaleString()}</time>
                <p className="body">{c.body}</p>
              </li>
            ))}
          </ul>
        )}
      </section>
    </main>
  );
}
