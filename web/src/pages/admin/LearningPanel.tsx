import { useState } from "react";
import type { Suggestion } from "../../api/admin";
import { ApiError } from "../../api/client";
import ErrorMessage, { describeError } from "../../components/ErrorMessage";
import { useDecideSuggestion, useSuggestions } from "../../hooks/useAdmin";

const PENDING = "PENDING_APPROVAL";

function describeDecision(err: unknown): string {
  if (err instanceof ApiError && err.status === 409) return "This suggestion was already decided.";
  return describeError(err);
}

const signed = (n: number) => (n > 0 ? `+${n}` : String(n));

function scope(s: Suggestion): string {
  if (s.event_pattern) return `Notifications of type ${s.event_pattern.event_type}${s.event_pattern.label ? ` labelled ${s.event_pattern.label}` : ""}`;
  return `Rule ${s.rule_id ?? "(unknown)"}`;
}

export default function LearningPanel() {
  const list = useSuggestions();
  const decide = useDecideSuggestion();
  const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  function act(s: Suggestion, decision: "approve" | "reject") {
    setBusy(s.id);
    decide.mutate(
      { id: s.id, decision },
      {
        onSuccess: () => setResult({ ok: true, text: `${decision === "approve" ? "Approved" : "Rejected"}: ${scope(s)}.` }),
        onError: (err) => setResult({ ok: false, text: `${scope(s)}: ${describeDecision(err)}` }),
        onSettled: () => setBusy(null),
      },
    );
  }

  const all = list.data ?? [];
  const pending = all.filter((s) => s.status === PENDING);
  const decided = all.filter((s) => s.status !== PENDING);

  return (
    <section aria-label="Learning suggestions">
      <h2>Learning suggestions</h2>
      <p className="meta">AgentPulse proposes score adjustments from how people react to notifications. Nothing changes until you approve.</p>
      {result && (
        <p role={result.ok ? "status" : "alert"} className={result.ok ? undefined : "error"}>
          {result.text}
        </p>
      )}
      {list.isPending && <p role="status">Loading suggestions...</p>}
      {list.isError && <ErrorMessage error={list.error} onRetry={() => void list.refetch()} />}
      {list.data && pending.length === 0 && <p>No suggestion is waiting for a decision.</p>}
      {pending.map((s) => (
        <article key={s.id} className="card" aria-label={`Suggestion for ${scope(s)}`}>
          <h3>{scope(s)}</h3>
          <p>
            Adjustment {signed(s.current_adjustment)} now, {signed(s.proposed_adjustment)} proposed.
          </p>
          <p className="meta">Evidence: {s.signal_count} feedback signals.</p>
          <blockquote aria-label="Rationale">{s.rationale}</blockquote>
          <div className="actions" role="group" aria-label="Decide">
            <button type="button" disabled={busy === s.id} onClick={() => act(s, "approve")}>
              Approve
            </button>
            <button type="button" disabled={busy === s.id} onClick={() => act(s, "reject")}>
              Reject
            </button>
          </div>
        </article>
      ))}
      {decided.length > 0 && (
        <>
          <h3>Decided</h3>
          <ul aria-label="Decided suggestions">
            {decided.map((s) => (
              <li key={s.id}>
                {scope(s)}: {signed(s.proposed_adjustment)}, {s.status.toLowerCase().replace(/_/g, " ")}
              </li>
            ))}
          </ul>
        </>
      )}
    </section>
  );
}
