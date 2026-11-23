import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, vi } from "vitest";
import AppRoutes from "../AppRoutes";
import { json, makeToken, meHandler, mockApi, renderWithProviders, signInAs } from "../test-utils";
import { SESSION_EXPIRED, SessionStore, tokenExpiry } from "./session";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

async function signInThroughTheForm(token: string) {
  const user = userEvent.setup();
  await user.type(screen.getByLabelText("Development token"), token);
  await user.click(screen.getByRole("button", { name: "Sign in" }));
}

describe("sign-in", () => {
  test("a signed-out visit lands on the sign-in page, and a token signs in and returns", async () => {
    mockApi(meHandler("member"));
    renderWithProviders(<AppRoutes />, { route: "/tasks" });
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();

    await signInThroughTheForm(makeToken());
    expect(await screen.findByRole("heading", { name: "Tasks" })).toBeInTheDocument();
    expect(screen.getByText(/usr_bob \(tenant_acme, member\)/)).toBeInTheDocument();
  });

  test("the token is sent as a bearer header and never written to storage", async () => {
    const api = mockApi(meHandler("member"));
    const store = new SessionStore();
    const token = makeToken();
    renderWithProviders(<AppRoutes />, { route: "/signin", store });
    await signInThroughTheForm(token);
    await screen.findByRole("heading", { name: "Tasks" });
    const me = api.calls.find((c) => c.path === "/api/v1/me")!;
    expect(me.headers.get("Authorization")).toBe(`Bearer ${token}`);
    expect(JSON.stringify({ ...localStorage })).not.toContain(token);
    expect(JSON.stringify({ ...sessionStorage })).not.toContain(token);
    expect(document.cookie).not.toContain(token);
  });

  test("a token the server refuses shows an error and signs nobody in", async () => {
    mockApi((c) => (c.path === "/api/v1/me" ? json(401, { error: { code: "unauthorized", message: "bad" } }) : undefined));
    const store = new SessionStore();
    renderWithProviders(<AppRoutes />, { route: "/signin", store });
    await signInThroughTheForm(makeToken());
    expect(await screen.findByRole("alert")).toHaveTextContent("did not accept that token");
    expect(store.getSnapshot().identity).toBeNull();
    expect(store.getToken()).toBeNull();
    // The refusal is not reported as an expired session.
    expect(screen.queryByText(SESSION_EXPIRED)).not.toBeInTheDocument();
  });

  test("a malformed or already expired token never reaches the server", async () => {
    const api = mockApi(meHandler());
    const store = new SessionStore();
    await expect(store.signIn("not a token")).rejects.toThrow("does not look like a token");
    await expect(store.signIn(makeToken({ exp: Math.floor(Date.now() / 1000) - 60 }))).rejects.toThrow("expired");
    expect(api.calls).toHaveLength(0);
  });

  test("an unreachable server is reported as such", async () => {
    vi.stubGlobal("fetch", async () => {
      throw new TypeError("network down");
    });
    await expect(new SessionStore().signIn(makeToken())).rejects.toThrow("Could not reach the server");
  });
});

describe("expired sessions", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "Date"], shouldAdvanceTime: true });
  });

  test("the session ends at the token's expiry and the user returns to sign-in with a notice", async () => {
    mockApi(meHandler("member"));
    const store = new SessionStore();
    renderWithProviders(<AppRoutes />, { route: "/tasks", store });
    await signInAs(store);
    expect(await screen.findByRole("heading", { name: "Tasks" })).toBeInTheDocument();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3600 * 1000);
    });
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent(SESSION_EXPIRED);
    expect(store.getToken()).toBeNull();
  });

  test("a 401 from any request ends the session the same way", async () => {
    const api = mockApi(meHandler("member"));
    const store = new SessionStore();
    renderWithProviders(<AppRoutes />, { route: "/tasks", store });
    await signInAs(store);
    await screen.findByRole("heading", { name: "Tasks" });

    api.add((c) => (c.path === "/api/v1/tasks" ? json(401, { error: { code: "unauthorized", message: "expired" } }) : undefined));
    const { api: client } = await import("../api/client");
    await act(async () => {
      await client.GET("/api/v1/tasks");
    });
    await waitFor(() => expect(screen.getByRole("heading", { name: "Sign in" })).toBeInTheDocument());
    expect(screen.getByRole("status")).toHaveTextContent(SESSION_EXPIRED);
  });

  test("signing out clears what the last person loaded", async () => {
    mockApi(meHandler("member"));
    const store = new SessionStore();
    const { client } = renderWithProviders(<AppRoutes />, { route: "/tasks", store });
    await signInAs(store);
    await screen.findByRole("heading", { name: "Tasks" });
    client.setQueryData(["tasks"], { secret: "other tenant" });
    act(() => store.signOut());
    await waitFor(() => expect(client.getQueryData(["tasks"])).toBeUndefined());
  });
});

describe("roles", () => {
  test("a member sees no admin navigation and is turned away from /admin", async () => {
    mockApi(meHandler("member"));
    const store = new SessionStore();
    renderWithProviders(<AppRoutes />, { route: "/admin", store });
    await signInAs(store, "member");
    expect(await screen.findByRole("heading", { name: "Tasks" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Admin" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Admin" })).not.toBeInTheDocument();
  });

  test("an admin sees the admin navigation and page", async () => {
    mockApi(meHandler("admin"));
    const store = new SessionStore();
    renderWithProviders(<AppRoutes />, { route: "/admin", store });
    await signInAs(store, "admin");
    expect(await screen.findByRole("heading", { name: "Admin" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Admin" })).toBeInTheDocument();
  });
});

describe("tokenExpiry", () => {
  test("reads exp, and refuses what is not a JWT", () => {
    expect(tokenExpiry(makeToken({ exp: 123 }))).toBe(123);
    expect(tokenExpiry("a.b")).toBeNull();
    expect(tokenExpiry("a.!!.c")).toBeNull();
    expect(tokenExpiry(["x", btoa('{"no":"exp"}'), "y"].join("."))).toBeNull();
  });
});
