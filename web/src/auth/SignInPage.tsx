import { useState, type FormEvent } from "react";
import { Navigate, useLocation, useNavigate } from "react-router-dom";
import { useAuth } from "./AuthProvider";

/**
 * Development sign-in: paste a token from `make dev-token` (cmd/devtoken).
 * The real identity-provider redirect waits on owner action O2.
 */
export default function SignInPage() {
  const { identity, notice, signIn } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const [token, setToken] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const from = (location.state as { from?: string } | null)?.from ?? "/tasks";

  if (identity) return <Navigate to={from} replace />;

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await signIn(token);
      navigate(from, { replace: true });
    } catch (err) {
      setError(err instanceof Error ? err.message : "Sign-in failed.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="signin">
      <h1>Taskify</h1>
      <h2>Sign in</h2>
      {notice && <p role="status">{notice}</p>}
      <form onSubmit={submit}>
        <label htmlFor="token">Development token</label>
        <textarea
          id="token"
          name="token"
          rows={5}
          value={token}
          onChange={(e) => setToken(e.target.value)}
          autoComplete="off"
          spellCheck={false}
          required
        />
        <p className="hint">Make one with <code>make dev-token</code>. It is kept in memory only, so a reload signs you out.</p>
        {error && <p role="alert">{error}</p>}
        <button type="submit" disabled={busy || token.trim() === ""}>
          {busy ? "Signing in..." : "Sign in"}
        </button>
      </form>
    </main>
  );
}
