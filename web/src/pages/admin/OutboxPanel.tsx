import { useState } from "react";
import { ApiError } from "../../api/client";
import { formatTime } from "../../api/agentText";
import ErrorMessage, { describeError } from "../../components/ErrorMessage";
import { useOutbox, useReplay } from "../../hooks/useAdmin";

/** Dead-letter rows: what could not be delivered to AgentPulse, with the last error, and a replay. */
export default function OutboxPanel() {
  const list = useOutbox("dead_letter");
  const replay = useReplay();
  const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  function act(id: string, eventType: string) {
    setBusy(id);
    replay.mutate(id, {
      onSuccess: () => setResult({ ok: true, text: `Queued ${eventType} for delivery again.` }),
      onError: (err) =>
        setResult({ ok: false, text: err instanceof ApiError && err.code === "not_dead_letter" ? "That row is no longer a dead letter." : describeError(err) }),
      onSettled: () => setBusy(null),
    });
  }

  const entries = list.data?.entries ?? [];
  return (
    <section aria-label="Dead letters">
      <h2>Dead letters</h2>
      <p className="meta">Notifications AgentPulse did not accept after every attempt. Replay one to send it again.</p>
      {result && (
        <p role={result.ok ? "status" : "alert"} className={result.ok ? undefined : "error"}>
          {result.text}
        </p>
      )}
      {list.isPending && <p role="status">Loading dead letters...</p>}
      {list.isError && <ErrorMessage error={list.error} onRetry={() => void list.refetch()} />}
      {list.data && entries.length === 0 && <p>No row is in the dead letter queue.</p>}
      {entries.length > 0 && (
        <table className="sidebyside">
          <thead>
            <tr>
              <th scope="col">Event</th>
              <th scope="col">Attempts</th>
              <th scope="col">Created</th>
              <th scope="col">Last error</th>
              <th scope="col">Action</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((e) => (
              <tr key={e.id}>
                <th scope="row">{e.event_type}</th>
                <td>
                  {e.attempts} of {e.max_attempts}
                </td>
                <td>{formatTime(e.created_at)}</td>
                <td>{e.last_error ?? "-"}</td>
                <td>
                  <button type="button" disabled={busy === e.id} onClick={() => act(e.id, e.event_type)} aria-label={`Replay ${e.event_type} ${e.id}`}>
                    Replay
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {list.data?.next && <p className="meta">More rows exist. Replay some of these to see them.</p>}
    </section>
  );
}
