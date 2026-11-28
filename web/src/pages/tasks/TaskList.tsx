import { Link } from "react-router-dom";
import { PRIORITY_LABEL, STATUS_LABEL, type Task } from "../../api/tasks";

export default function TaskList({ tasks }: { tasks: Task[] }) {
  return (
    <table className="tasks">
      <thead>
        <tr>
          <th scope="col">Title</th>
          <th scope="col">Status</th>
          <th scope="col">Priority</th>
          <th scope="col">Assignee</th>
          <th scope="col">Updated</th>
        </tr>
      </thead>
      <tbody>
        {tasks.map((t) => (
          <tr key={t.id}>
            <td>
              <Link to={`/tasks/${t.id}`}>{t.title}</Link>
            </td>
            <td>{STATUS_LABEL[t.status]}</td>
            <td>{PRIORITY_LABEL[t.priority]}</td>
            <td>{t.assignee_id ?? "Unassigned"}</td>
            <td>
              <time dateTime={t.updated_at}>{new Date(t.updated_at).toLocaleString()}</time>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
