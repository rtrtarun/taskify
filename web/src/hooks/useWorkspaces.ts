import { useQuery } from "@tanstack/react-query";
import { fetchWorkspaces } from "../api/workspaces";

/** The tenant's workspaces. They change rarely, so they are not polled. */
export function useWorkspaces() {
  return useQuery({ queryKey: ["workspaces"], queryFn: fetchWorkspaces, staleTime: 60_000 });
}
