import { Link } from "react-router-dom";

export interface GraphNode {
  id: string;
  title: string;
  status: string;
}

/** from must be finished before to can start. */
export interface GraphEdge {
  from: string;
  to: string;
}

export interface PlacedNode extends GraphNode {
  layer: number;
  row: number;
}

/**
 * layoutGraph puts each node in a layer: layer 0 has nothing to wait for,
 * and every other node sits one layer after the latest node it waits for.
 * Edges to nodes that are not in the list are ignored. The server refuses
 * cycles; if one arrived anyway, the nodes on it are placed as if the edge
 * that closes it were not there, so the layout still ends.
 */
export function layoutGraph(nodes: GraphNode[], edges: GraphEdge[]): { nodes: PlacedNode[]; edges: GraphEdge[]; layers: number } {
  const ids = new Set(nodes.map((n) => n.id));
  const kept = edges.filter((e) => ids.has(e.from) && ids.has(e.to) && e.from !== e.to);
  const waitsFor = new Map<string, string[]>();
  for (const e of kept) waitsFor.set(e.to, [...(waitsFor.get(e.to) ?? []), e.from]);

  const layerOf = new Map<string, number>();
  const visiting = new Set<string>();
  const layer = (id: string): number => {
    const known = layerOf.get(id);
    if (known !== undefined) return known;
    if (visiting.has(id)) return 0; // a cycle: do not follow it
    visiting.add(id);
    const l = Math.max(-1, ...(waitsFor.get(id) ?? []).map(layer)) + 1;
    visiting.delete(id);
    layerOf.set(id, l);
    return l;
  };

  const rowsUsed: number[] = [];
  const placed = nodes.map((n) => {
    const l = layer(n.id);
    const row = rowsUsed[l] ?? 0;
    rowsUsed[l] = row + 1;
    return { ...n, layer: l, row };
  });
  return { nodes: placed, edges: kept, layers: rowsUsed.length };
}

const W = 168;
const H = 38;
const GAP_X = 64;
const GAP_Y = 14;
const PAD = 10;

const clip = (s: string, n = 22) => (s.length > n ? s.slice(0, n - 1) + "…" : s);

/**
 * DependencyGraph draws a set of tasks and their "waits for" links, left to
 * right: a task is to the right of everything it waits for. Titles are
 * drawn as text. The same information is in the list beside it on the
 * page, which is what a screen reader should use.
 */
export default function DependencyGraph({ nodes, edges }: { nodes: GraphNode[]; edges: GraphEdge[] }) {
  const g = layoutGraph(nodes, edges);
  const rows = Math.max(1, ...g.nodes.map((n) => n.row + 1));
  const width = PAD * 2 + g.layers * W + Math.max(0, g.layers - 1) * GAP_X;
  const height = PAD * 2 + rows * H + (rows - 1) * GAP_Y;
  const pos = new Map(g.nodes.map((n) => [n.id, { x: PAD + n.layer * (W + GAP_X), y: PAD + n.row * (H + GAP_Y) }]));
  const done = new Set(g.nodes.filter((n) => n.status === "completed").map((n) => n.id));

  return (
    <svg
      className="dep-graph"
      role="img"
      aria-label={`Dependency graph: ${g.nodes.length} subtasks, ${g.edges.length} links`}
      viewBox={`0 0 ${width} ${height}`}
      width={width}
      height={height}
    >
      <defs>
        <marker id="dep-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
          <path d="M 0 0 L 10 5 L 0 10 z" fill="currentColor" />
        </marker>
      </defs>
      {g.edges.map((e) => {
        const a = pos.get(e.from)!;
        const b = pos.get(e.to)!;
        const x1 = a.x + W;
        const y1 = a.y + H / 2;
        const x2 = b.x;
        const y2 = b.y + H / 2;
        const mid = (x1 + x2) / 2;
        return (
          <path
            key={`${e.from}>${e.to}`}
            data-testid="edge"
            data-from={e.from}
            data-to={e.to}
            d={`M ${x1} ${y1} C ${mid} ${y1}, ${mid} ${y2}, ${x2} ${y2}`}
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeDasharray={done.has(e.from) ? "4 3" : undefined}
            markerEnd="url(#dep-arrow)"
          />
        );
      })}
      {g.nodes.map((n) => {
        const p = pos.get(n.id)!;
        return (
          <g key={n.id} data-testid="node" data-layer={n.layer} data-status={n.status} transform={`translate(${p.x} ${p.y})`}>
            <rect width={W} height={H} rx="6" className={`dep-node dep-${n.status}`} />
            <Link to={`/tasks/${n.id}`}>
              <text x={10} y={H / 2 + 4} className="dep-label">
                {clip(n.title)}
                <title>{n.title}</title>
              </text>
            </Link>
          </g>
        );
      })}
    </svg>
  );
}
