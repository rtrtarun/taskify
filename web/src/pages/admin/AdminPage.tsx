import { NavLink, Navigate, Route, Routes } from "react-router-dom";
import JobsPanel from "./JobsPanel";
import LearningPanel from "./LearningPanel";
import OutboxPanel from "./OutboxPanel";
import WorkspacePanel from "./WorkspacePanel";

/**
 * The admin pages. The server refuses a member's admin requests itself, so
 * this is only the layout; RequireAdmin keeps members from the links.
 */
export default function AdminPage() {
  return (
    <main>
      <h1>Admin</h1>
      <nav aria-label="Admin sections" className="subnav">
        <NavLink to="/admin/learning">Learning</NavLink>
        <NavLink to="/admin/delivery">Delivery</NavLink>
        <NavLink to="/admin/jobs">Agent jobs</NavLink>
        <NavLink to="/admin/workspaces">Workspaces</NavLink>
      </nav>
      <Routes>
        <Route index element={<Navigate to="learning" replace />} />
        <Route path="learning" element={<LearningPanel />} />
        <Route path="delivery" element={<OutboxPanel />} />
        <Route path="jobs" element={<JobsPanel />} />
        <Route path="workspaces" element={<WorkspacePanel />} />
        <Route path="*" element={<Navigate to="learning" replace />} />
      </Routes>
    </main>
  );
}
