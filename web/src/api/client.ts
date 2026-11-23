import createClient, { type Middleware } from "openapi-fetch";
import type { components, paths } from "./schema";

export type Schemas = components["schemas"];

/** The API error body, as contract/openapi.yaml defines it. */
export type ErrorBody = Schemas["Error"];

/**
 * ApiError is a non-2xx answer. Callers branch on `code` and `status`, never
 * on `message` (which is for people).
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly details: Record<string, unknown>;
  /** Seconds from a Retry-After header, when the server sent one. */
  readonly retryAfter: number | undefined;

  constructor(status: number, body: Partial<ErrorBody> | undefined, retryAfter?: number) {
    super(body?.error?.message ?? `request failed with status ${status}`);
    this.name = "ApiError";
    this.status = status;
    this.code = body?.error?.code ?? "unknown";
    this.details = (body?.error?.details as Record<string, unknown> | undefined) ?? {};
    this.retryAfter = retryAfter;
  }
}

/** Hooks the auth layer installs; the client never reads the token itself. */
export interface AuthHooks {
  getToken: () => string | null;
  /** Called when the server answers 401 to a request that carried a token. */
  onUnauthorized: () => void;
}

let hooks: AuthHooks = { getToken: () => null, onUnauthorized: () => {} };

export function setAuthHooks(h: AuthHooks): void {
  hooks = h;
}

const authMiddleware: Middleware = {
  onRequest({ request }) {
    const token = hooks.getToken();
    if (token) request.headers.set("Authorization", `Bearer ${token}`);
    return request;
  },
  onResponse({ request, response }) {
    if (response.status === 401 && request.headers.has("Authorization")) hooks.onUnauthorized();
    return response;
  },
};

/** The typed client. Paths already carry the /api/v1 prefix. */
// The fetch is looked up on each call so tests can replace it.
export const api = createClient<paths>({ baseUrl: globalThis.location?.origin ?? "http://localhost", fetch: (request) => globalThis.fetch(request) });
api.use(authMiddleware);

/**
 * unwrap returns the data of an openapi-fetch result or throws an ApiError.
 * Callers handle a 304 themselves, before calling unwrap.
 */
export function unwrap<T>(result: { data?: T; error?: unknown; response: Response }): T {
  const { data, error, response } = result;
  if (response.ok && data !== undefined) return data;
  const retry = Number(response.headers.get("Retry-After"));
  throw new ApiError(response.status, error as Partial<ErrorBody> | undefined, Number.isFinite(retry) && retry > 0 ? retry : undefined);
}
