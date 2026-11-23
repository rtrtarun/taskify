import { ApiError } from "../api/client";

/** A message for an error, by its code, never by parsing the server's text. */
export function describeError(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case "version_conflict":
        return "Someone else changed this first. Your change was not saved.";
      case "rate_limited":
        return "Too many requests. Wait a moment and try again.";
      case "unauthorized":
        return "Your session has ended. Sign in again.";
      case "forbidden":
        return "You are not allowed to do that.";
      case "not_found":
        return "That no longer exists, or you cannot see it.";
      case "upstream_unavailable":
        return "That service is unavailable right now. Try again shortly.";
      default:
        // validation_failed, invalid_transition, and the rest carry a message
        // written for people.
        return err.message;
    }
  }
  return "Something went wrong. Check your connection and try again.";
}

export default function ErrorMessage({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  return (
    <div role="alert" className="error">
      <p>{describeError(error)}</p>
      {onRetry && (
        <button type="button" onClick={onRetry}>
          Try again
        </button>
      )}
    </div>
  );
}
