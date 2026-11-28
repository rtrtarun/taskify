import { api, unwrap, type Schemas } from "./client";

export type InAppNotification = Schemas["InAppNotification"];
export type NotificationFeed = Schemas["NotificationFeed"];
export type AckSignal = Schemas["AckSignal"];

/** One page of the caller's feed. The browser asks Taskify, never AgentPulse. */
export async function fetchFeed(cursor?: string): Promise<NotificationFeed> {
  return unwrap(await api.GET("/api/v1/notifications/in-app", { params: { query: { cursor, limit: 20 } }, cache: "no-store" }));
}

export async function ackNotification(id: string, signal: AckSignal, duration?: string): Promise<Schemas["NotificationAck"]> {
  return unwrap(
    await api.POST("/api/v1/notifications/{id}/ack", { params: { path: { id } }, body: duration ? { signal, duration } : { signal } }),
  );
}
