import { NavLink, Outlet } from "react-router-dom";
import { BUILT_API_VERSION, useNewerServerVersion } from "../api/version";
import { useAuth } from "../auth/AuthProvider";

export default function Layout() {
  const { identity, isAdmin, signOut } = useAuth();
  const newer = useNewerServerVersion();
  return (
    <>
      <header className="topbar">
        <strong>Taskify</strong>
        <nav aria-label="Main">
          <NavLink to="/tasks">Tasks</NavLink>
          <NavLink to="/approvals">Approvals</NavLink>
          <NavLink to="/notifications">Notifications</NavLink>
          {isAdmin && <NavLink to="/admin">Admin</NavLink>}
        </nav>
        <span className="who">
          {identity?.userId} ({identity?.tenantId}, {identity?.role})
        </span>
        <button type="button" onClick={signOut}>
          Sign out
        </button>
      </header>
      {newer && (
        <p role="status" className="notice">
          The server is newer than this app (API {newer}, app built for {BUILT_API_VERSION}). Reload the page to get the latest version.
        </p>
      )}
      <Outlet />
    </>
  );
}
