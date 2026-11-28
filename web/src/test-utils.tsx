import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { ReactElement } from "react";
import { MemoryRouter } from "react-router-dom";
import { vi } from "vitest";
import { AuthProvider } from "./auth/AuthProvider";
import { SessionStore } from "./auth/session";

export function json(status: number, body?: unknown, headers: Record<string, string> = {}): Response {
  if (status === 204 || status === 304) return new Response(null, { status, headers });
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });
}

export interface Call {
  method: string;
  path: string;
  url: URL;
  headers: Headers;
  body: unknown;
}

type Handler = (call: Call) => Response | Promise<Response> | undefined;

/**
 * mockApi replaces fetch. Handlers are tried in order; the first to return a
 * Response answers. An unanswered request fails the test loudly with a 599.
 */
export function mockApi(...handlers: Handler[]): { calls: Call[]; add: (h: Handler) => void } {
  const calls: Call[] = [];
  const list = [...handlers];
  vi.stubGlobal("fetch", async (input: Request) => {
    const url = new URL(input.url, "http://localhost");
    const text = await input.clone().text();
    const call: Call = {
      method: input.method,
      path: url.pathname,
      url,
      headers: input.headers,
      body: text ? JSON.parse(text) : undefined,
    };
    calls.push(call);
    for (const h of list) {
      const res = await h(call);
      if (res) return res;
    }
    return json(599, { error: { code: "unmocked", message: `${call.method} ${call.path}` } });
  });
  return { calls, add: (h) => list.unshift(h) };
}

function b64url(s: string): string {
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** A JWT-shaped token. The server verifies signatures; the web app does not. */
export function makeToken(claims: { exp?: number; role?: string; sub?: string; tenant_id?: string } = {}): string {
  const payload = { sub: "usr_bob", tenant_id: "tenant_acme", role: "member", exp: Math.floor(Date.now() / 1000) + 3600, ...claims };
  return [b64url(JSON.stringify({ alg: "HS256", typ: "JWT" })), b64url(JSON.stringify(payload)), "sig"].join(".");
}

export function meHandler(role: "admin" | "member" = "member", user = "usr_bob", tenant = "tenant_acme"): Handler {
  return (c) => (c.path === "/api/v1/me" ? json(200, { tenant_id: tenant, user_id: user, role }) : undefined);
}

export function testClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity }, mutations: { retry: false } } });
}

export function renderWithProviders(ui: ReactElement, opts: { route?: string; store?: SessionStore; client?: QueryClient } = {}) {
  const store = opts.store ?? new SessionStore();
  const client = opts.client ?? testClient();
  const result = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[opts.route ?? "/"]}>
        <AuthProvider store={store}>{ui}</AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { ...result, store, client };
}

/** Signs a store in as if the user had pasted a token. */
export async function signInAs(store: SessionStore, role: "admin" | "member" = "member"): Promise<void> {
  await store.signIn(makeToken({ role }));
}
