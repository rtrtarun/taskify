import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { ApiError } from "../../api/client";
import type { InAppNotification } from "../../api/notifications";
import ErrorMessage from "../../components/ErrorMessage";
import { useAck, useFeed } from "../../hooks/useNotifications";
import { STATUS_LABEL } from "../../api/tasks";

/** A notification is handled once the person acted on it or dismissed it, or while it is snoozed. */
function handled(n: InAppNotification, now: number): boolean {
  if (n.signal === "snoozed") return n.snoozed_until !== null && new Date(n.snoozed_until).getTime() > now;
  return n.read;
}

export default function NotificationsPage({ pollMs, now = Date.now }: { pollMs?: number; now?: () => number }) {
  const feed = useFeed(pollMs);
  const ack = useAck();
  const navigate = useNavigate();
  const [showHandled, setShowHandled] = useState(false);

  const all = feed.data?.pages.flatMap((p) => p.notifications) ?? [];
  const shown = showHandled ? all : all.filter((n) => !handled(n, now()));
  const unavailable = feed.error instanceof ApiError && feed.error.code === "upstream_unavailable";

  return (
    <main>
      <h1>Notifications</h1>

      {feed.isPending && <p role="status">Loading notifications...</p>}
      {feed.isError && !feed.data && (
        <div role="alert" className="error">
          <p>
            {unavailable
              ? "Notifications are unavailable right now. Your tasks are not affected. This page tries again on its own."
              : "Notifications could not be loaded."}
          </p>
          <button type="button" onClick={() => void feed.refetch()}>
            Try again
          </button>
        </div>
      )}
      {feed.isError && feed.data && <p role="status">Could not refresh notifications. Showing the last ones.</p>}
      {ack.isError && <ErrorMessage error={ack.error} />}

      {feed.data && (
        <>
          <label>
            <input type="checkbox" checked={showHandled} onChange={(e) => setShowHandled(e.target.checked)} />
            Show handled
          </label>
          {shown.length === 0 && <p>{all.length === 0 ? "You have no notifications." : "Nothing new. Everything here is handled."}</p>}
          <ul className="notifications">
            {shown.map((n) => (
              <li key={n.id} className={handled(n, now()) ? "handled" : undefined}>
                <h2>{n.title}</h2>
                <p className="body">{n.body}</p>
                <p className="meta">
                  <time dateTime={n.created_at}>{new Date(n.created_at).toLocaleString()}</time>
                  {n.task && (
                    <>
                      {" - "}
                      <Link to={`/tasks/${n.task.id}`}>{n.task.title}</Link> ({STATUS_LABEL[n.task.status]})
                    </>
                  )}
                  {n.signal && ` - ${n.signal === "snoozed" ? "snoozed" : n.signal}`}
                </p>
                <div role="group" aria-label={`Actions for ${n.title}`}>
                  <button
                    type="button"
                    disabled={ack.isPending}
                    onClick={() =>
                      ack.mutate(
                        { id: n.id, signal: "acted" },
                        { onSuccess: () => n.task_id && navigate(`/tasks/${n.task_id}`) },
                      )
                    }
                  >
                    {n.task_id ? "Open" : "Done"}
                  </button>
                  <button type="button" disabled={ack.isPending} onClick={() => ack.mutate({ id: n.id, signal: "snoozed", duration: "2h" })}>
                    Snooze 2h
                  </button>
                  <button type="button" disabled={ack.isPending} onClick={() => ack.mutate({ id: n.id, signal: "dismissed" })}>
                    Dismiss
                  </button>
                </div>
              </li>
            ))}
          </ul>
          {feed.hasNextPage && (
            <button type="button" onClick={() => void feed.fetchNextPage()} disabled={feed.isFetchingNextPage}>
              Load more
            </button>
          )}
        </>
      )}
    </main>
  );
}
