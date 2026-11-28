import { NavLink, Outlet } from "react-router-dom";
import { useAuth } from "../auth/AuthProvider";

export default function Layout() {
  const { identity, isAdmin, signOut } = useAuth();
  return (
    <>
      <header className="topbar">
        <strong>Taskify</strong>
        <nav aria-label="Main">
          <NavLink to="/tasks">Tasks</NavLink>
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
      <Outlet />
    </>
  );
}
