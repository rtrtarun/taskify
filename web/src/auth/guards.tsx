import type { ReactNode } from "react";
import { Navigate, useLocation } from "react-router-dom";
import { useAuth } from "./AuthProvider";

/** RequireAuth sends a signed-out user to sign-in and brings them back after. */
export function RequireAuth({ children }: { children: ReactNode }) {
  const { identity } = useAuth();
  const location = useLocation();
  if (!identity) return <Navigate to="/signin" replace state={{ from: location.pathname + location.search }} />;
  return <>{children}</>;
}

/**
 * RequireAdmin keeps the admin pages from a member. This is navigation, not
 * security: the server refuses a member's admin requests itself.
 */
export function RequireAdmin({ children }: { children: ReactNode }) {
  const { isAdmin } = useAuth();
  if (!isAdmin) return <Navigate to="/tasks" replace />;
  return <>{children}</>;
}
