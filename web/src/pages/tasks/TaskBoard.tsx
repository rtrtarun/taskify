import { useState } from "react";
import { Link } from "react-router-dom";
import { BOARD_STATUSES, PRIORITY_LABEL, STATUS_LABEL, type Task, type TaskStatus } from "../../api/tasks";

interface Props {
  tasks: Task[];
  /** Asks the server to move a task; the board shows whatever the server decides. */
  onMove: (task: Task, status: TaskStatus) => void;
}

/**
 * One column per status. A card moves by dragging it to another column, or,
 * for keyboards, touch, and screen readers, with its "Move to" control.
 */
export default function TaskBoard({ tasks, onMove }: Props) {
  const [over, setOver] = useState<TaskStatus | null>(null);
  const byId = new Map(tasks.map((t) => [t.id, t]));

  return (
    <div className="board">
      {BOARD_STATUSES.map((status) => {
        const column = tasks.filter((t) => t.status === status);
        return (
          <section
            key={status}
            aria-labelledby={`col-${status}`}
            className={over === status ? "column over" : "column"}
            onDragOver={(e) => {
              e.preventDefault();
              setOver(status);
            }}
            onDragLeave={() => setOver(null)}
            onDrop={(e) => {
              e.preventDefault();
              setOver(null);
              const task = byId.get(e.dataTransfer.getData("text/plain"));
              if (task && task.status !== status) onMove(task, status);
            }}
          >
            <h2 id={`col-${status}`}>
              {STATUS_LABEL[status]} <span className="count">({column.length})</span>
            </h2>
            <ul>
              {column.map((t) => (
                <li key={t.id} draggable onDragStart={(e) => e.dataTransfer.setData("text/plain", t.id)}>
                  <Link to={`/tasks/${t.id}`}>{t.title}</Link>
                  <div className="meta">
                    {PRIORITY_LABEL[t.priority]} - {t.assignee_id ?? "Unassigned"}
                  </div>
                  <select value={t.status} onChange={(e) => onMove(t, e.target.value as TaskStatus)} aria-label={`Move ${t.title} to`}>
                    {BOARD_STATUSES.map((s) => (
                      <option key={s} value={s}>
                        {STATUS_LABEL[s]}
                      </option>
                    ))}
                  </select>
                </li>
              ))}
            </ul>
          </section>
        );
      })}
    </div>
  );
}
