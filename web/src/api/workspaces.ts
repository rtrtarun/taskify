import { api, unwrap, type Schemas } from "./client";

export type Workspace = Schemas["Workspace"];

/** Most pages followed for one list, so a runaway cursor cannot loop forever. */
const MAX_PAGES = 10;

/** fetchWorkspaces reads the caller's tenant's workspaces, following the cursor. */
export async function fetchWorkspaces(): Promise<Workspace[]> {
  const out: Workspace[] = [];
  let cursor: string | undefined;
  for (let i = 0; i < MAX_PAGES; i++) {
    const page = unwrap(await api.GET("/api/v1/workspaces", { params: { query: { limit: 200, cursor } }, cache: "no-store" }));
    out.push(...page.workspaces);
    if (!page.next_cursor) break;
    cursor = page.next_cursor;
  }
  return out;
}
