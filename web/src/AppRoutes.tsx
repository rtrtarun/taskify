import { Navigate, Route, Routes } from "react-router-dom";
import { RequireAdmin, RequireAuth } from "./auth/guards";
import SignInPage from "./auth/SignInPage";
import Layout from "./components/Layout";
import AdminPage from "./pages/admin/AdminPage";
import ApprovalsPage from "./pages/approvals/ApprovalsPage";
import BriefPage from "./pages/brief/BriefPage";
import NotificationsPage from "./pages/notifications/NotificationsPage";
import TaskDetailPage from "./pages/task-detail/TaskDetailPage";
import TasksPage from "./pages/tasks/TasksPage";

export default function AppRoutes() {
  return (
    <Routes>
      <Route path="/signin" element={<SignInPage />} />
      <Route
        element={
          <RequireAuth>
            <Layout />
          </RequireAuth>
        }
      >
        <Route path="/" element={<Navigate to="/tasks" replace />} />
        <Route path="/tasks" element={<TasksPage />} />
        <Route path="/tasks/:id" element={<TaskDetailPage />} />
        <Route path="/approvals" element={<ApprovalsPage />} />
        <Route path="/brief" element={<BriefPage />} />
        <Route path="/notifications" element={<NotificationsPage />} />
        <Route
          path="/admin/*"
          element={
            <RequireAdmin>
              <AdminPage />
            </RequireAdmin>
          }
        />
        <Route path="*" element={<Navigate to="/tasks" replace />} />
      </Route>
    </Routes>
  );
}
