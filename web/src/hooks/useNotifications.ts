import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api/client";
import { ackNotification, fetchFeed, type AckSignal } from "../api/notifications";

export const FEED_POLL_MS = 30_000;

export function useFeed(pollMs: number = FEED_POLL_MS) {
  return useInfiniteQuery({
    queryKey: ["notifications", "feed"],
    queryFn: ({ pageParam }) => fetchFeed(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    refetchInterval: pollMs,
    // A 503 means AgentPulse is down: the polling interval is the retry.
    retry: (n, err) => !(err instanceof ApiError) && n < 2,
  });
}

export function useAck() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { id: string; signal: AckSignal; duration?: string }) => ackNotification(v.id, v.signal, v.duration),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["notifications", "feed"] }),
  });
}
