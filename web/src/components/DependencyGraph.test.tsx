import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { Subtask } from "../api/agents";
import { SubtaskView } from "../pages/task-detail/Subtasks";
import DependencyGraph, { layoutGraph, type GraphEdge, type GraphNode } from "./DependencyGraph";

const node = (id: string, status = "todo"): GraphNode => ({ id, title: `Task ${id}`, status });
const edge = (from: string, to: string): GraphEdge => ({ from, to });

describe("layoutGraph", () => {
  test("no nodes, no layers", () => {
    expect(layoutGraph([], [])).toEqual({ nodes: [], edges: [], layers: 0 });
  });

  test("a chain is one task per layer", () => {
    const g = layoutGraph([node("a"), node("b"), node("c")], [edge("a", "b"), edge("b", "c")]);
    expect(g.nodes.map((n) => [n.id, n.layer, n.row])).toEqual([["a", 0, 0], ["b", 1, 0], ["c", 2, 0]]);
    expect(g.layers).toBe(3);
  });

  test("a diamond puts the two middle tasks in one layer and the last after both", () => {
    const g = layoutGraph(
      [node("a"), node("b"), node("c"), node("d")],
      [edge("a", "b"), edge("a", "c"), edge("b", "d"), edge("c", "d")],
    );
    expect(g.nodes.map((n) => [n.id, n.layer, n.row])).toEqual([["a", 0, 0], ["b", 1, 0], ["c", 1, 1], ["d", 2, 0]]);
  });

  test("a task sits after the latest task it waits for", () => {
    const g = layoutGraph([node("a"), node("b"), node("c")], [edge("a", "c"), edge("a", "b"), edge("b", "c")]);
    expect(g.nodes.find((n) => n.id === "c")!.layer).toBe(2);
  });

  test("links to tasks outside the list, and to itself, are dropped", () => {
    const g = layoutGraph([node("a")], [edge("x", "a"), edge("a", "a")]);
    expect(g.edges).toEqual([]);
    expect(g.nodes[0].layer).toBe(0);
  });

  test("a cycle, which the server refuses, still ends", () => {
    const g = layoutGraph([node("a"), node("b")], [edge("a", "b"), edge("b", "a")]);
    expect(g.nodes).toHaveLength(2);
  });
});

function subtask(id: string, dependsOn: string[] = [], status = "todo", title = `Task ${id}`): Subtask {
  return {
    task: {
      id, tenant_id: "t", workspace_id: "ws", title, description: "", status, priority: "p3_medium", creator_id: "u", assignee_id: null,
      labels: [], metadata: {}, watcher_ids: [], due_at: null, remind_at: null, completed_at: null, archived_at: null,
      blocked_reason: null, version: 1, created_at: "2026-11-20T10:00:00Z", updated_at: "2026-11-20T10:00:00Z",
    },
    depends_on: dependsOn,
  } as Subtask;
}

function view(subtasks: Subtask[]) {
  return render(
    <MemoryRouter>
      <SubtaskView subtasks={subtasks} />
    </MemoryRouter>,
  );
}

describe("subtasks of a task", () => {
  test("a task with no children says so and draws no graph", () => {
    view([]);
    expect(screen.getByText("No subtasks yet.")).toBeInTheDocument();
    expect(screen.queryByRole("img")).toBeNull();
    expect(screen.queryByRole("list")).toBeNull();
  });

  test("children with no links between them are a list, with no graph", () => {
    view([subtask("a"), subtask("b")]);
    expect(within(screen.getByRole("list", { name: "Subtasks" })).getAllByRole("listitem")).toHaveLength(2);
    expect(screen.queryByRole("img")).toBeNull();
  });

  test("a chain: three nodes in three layers, two arrows, and the list says who waits", () => {
    view([subtask("a", [], "completed"), subtask("b", ["a"], "todo"), subtask("c", ["b"], "blocked")]);
    const graph = screen.getByRole("img", { name: "Dependency graph: 3 subtasks, 2 links" });
    const nodes = within(graph).getAllByTestId("node");
    expect(nodes.map((n) => n.getAttribute("data-layer"))).toEqual(["0", "1", "2"]);
    expect(within(graph).getAllByTestId("edge").map((e) => `${e.getAttribute("data-from")}>${e.getAttribute("data-to")}`)).toEqual(["a>b", "b>c"]);
    const items = within(screen.getByRole("list", { name: "Subtasks" })).getAllByRole("listitem");
    expect(items[0]).toHaveTextContent("Task a - Completed");
    expect(items[1]).toHaveTextContent("waits for Task a");
    expect(items[2]).toHaveTextContent("waits for Task b");
    expect(within(graph).getAllByRole("link").map((l) => l.getAttribute("href"))).toEqual(["/tasks/a", "/tasks/b", "/tasks/c"]);
  });

  test("a diamond: four nodes, four arrows, the last waits for both middles", () => {
    view([subtask("a"), subtask("b", ["a"]), subtask("c", ["a"]), subtask("d", ["b", "c"], "blocked")]);
    const graph = screen.getByRole("img", { name: "Dependency graph: 4 subtasks, 4 links" });
    expect(within(graph).getAllByTestId("node").map((n) => n.getAttribute("data-layer"))).toEqual(["0", "1", "1", "2"]);
    expect(within(graph).getAllByTestId("edge")).toHaveLength(4);
    expect(within(screen.getByRole("list", { name: "Subtasks" })).getAllByRole("listitem")[3]).toHaveTextContent("waits for Task b, Task c");
  });

  test("a link to a task outside the list is counted in words, not drawn", () => {
    view([subtask("a", ["elsewhere"])]);
    expect(screen.queryByRole("img")).toBeNull();
    expect(screen.getByRole("listitem")).toHaveTextContent("and 1 other task");
  });

  test("titles are text, never markup", () => {
    const evil = '<img src=x onerror="alert(1)">';
    const { container } = view([subtask("a", [], "todo", evil), subtask("b", ["a"], "todo", "<b>bold</b>")]);
    expect(container.querySelector("img, b")).toBeNull();
    expect(screen.getAllByText(evil, { exact: false }).length).toBeGreaterThan(0);
  });
});

describe("DependencyGraph", () => {
  test("marks the arrows from finished tasks", () => {
    render(
      <MemoryRouter>
        <DependencyGraph nodes={[node("a", "completed"), node("b")]} edges={[edge("a", "b")]} />
      </MemoryRouter>,
    );
    expect(screen.getByTestId("edge")).toHaveAttribute("stroke-dasharray");
  });
});
