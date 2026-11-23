import { api, ApiError, unwrap } from "../api/client";

/**
 * The session: who is signed in, and the bearer token the API client sends.
 *
 * The token lives in this object's memory and nowhere else: not in local or
 * session storage, not in a cookie. A page reload therefore signs the user
 * out. (The task asks for an HttpOnly refresh cookie to survive reloads; that
 * needs a refresh endpoint the API does not have, which is a product
 * decision. See docs/implementation/r3-clients.md, M14-03.)
 */

export type Role = "admin" | "member";

export interface Identity {
  tenantId: string;
  userId: string;
  role: Role;
}

export interface SessionState {
  identity: Identity | null;
  /** Why the user is signed out, shown on the sign-in page. */
  notice: string | null;
}

/** What the server must see before the token's own expiry, in ms. */
const EXPIRY_MARGIN_MS = 5_000;
/** setTimeout cannot wait longer than 2^31 - 1 ms. */
const MAX_TIMER_MS = 2_147_483_647;

export const SESSION_EXPIRED = "Your session expired. Sign in again.";

/** Reads the `exp` claim (seconds since the epoch) of a JWT, unverified. The
 * server is the only judge of a token; this only schedules the sign-out. */
export function tokenExpiry(token: string): number | null {
  const parts = token.split(".");
  if (parts.length !== 3) return null;
  try {
    const json = atob(parts[1].replace(/-/g, "+").replace(/_/g, "/"));
    const exp = (JSON.parse(json) as { exp?: unknown }).exp;
    return typeof exp === "number" ? exp : null;
  } catch {
    return null;
  }
}

export class SessionStore {
  private token: string | null = null;
  private state: SessionState = { identity: null, notice: null };
  private listeners = new Set<() => void>();
  private timer: ReturnType<typeof setTimeout> | undefined;
  private signingIn = false;

  constructor(private now: () => number = Date.now) {}

  getToken = (): string | null => this.token;

  getSnapshot = (): SessionState => this.state;

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  private set(state: SessionState): void {
    this.state = state;
    this.listeners.forEach((l) => l());
  }

  /**
   * signIn accepts a token (from cmd/devtoken until an identity provider is
   * chosen, O2), asks the server who it is, and starts the session. It throws
   * an Error whose message is fit to show.
   */
  async signIn(raw: string): Promise<Identity> {
    const token = raw.trim();
    const exp = tokenExpiry(token);
    if (exp === null) throw new Error("That does not look like a token.");
    if (exp * 1000 <= this.now()) throw new Error("That token has expired.");

    this.token = token;
    this.signingIn = true;
    try {
      // The server judges the token and says who it makes the caller: the
      // tenant and role the rest of the app trusts for navigation. The
      // server still enforces access on every request.
      const me = unwrap(await api.GET("/api/v1/me"));
      const identity: Identity = {
        tenantId: me.tenant_id,
        userId: me.user_id,
        role: me.role === "admin" ? "admin" : "member",
      };
      this.set({ identity, notice: null });
      this.schedule(exp);
      return identity;
    } catch (e) {
      this.token = null;
      if (e instanceof ApiError && e.status === 401) throw new Error("The server did not accept that token.", { cause: e });
      throw new Error("Could not reach the server. Try again.", { cause: e });
    } finally {
      this.signingIn = false;
    }
  }

  /** signOut ends the session. notice explains why, when the user did not choose it. */
  signOut(notice: string | null = null): void {
    clearTimeout(this.timer);
    this.token = null;
    this.set({ identity: null, notice });
  }

  /** The server refused the token (401): the session is over. */
  expire = (): void => {
    // A 401 to the sign-in check itself is a refused token, reported by signIn.
    if (this.token !== null && !this.signingIn) this.signOut(SESSION_EXPIRED);
  };

  private schedule(exp: number): void {
    clearTimeout(this.timer);
    const wait = Math.min(Math.max(exp * 1000 - this.now() - EXPIRY_MARGIN_MS, 0), MAX_TIMER_MS);
    this.timer = setTimeout(() => {
      // A very long token re-arms itself rather than firing early.
      if (exp * 1000 - this.now() > EXPIRY_MARGIN_MS) this.schedule(exp);
      else this.expire();
    }, wait);
  }
}

/** The one session of the running app. */
export const session = new SessionStore();
