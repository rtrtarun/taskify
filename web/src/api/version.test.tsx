import { act, render, screen } from "@testing-library/react";
import { afterEach, vi } from "vitest";
import { SessionStore } from "../auth/session";
import Layout from "../components/Layout";
import { api } from "./client";
import { BUILT_API_VERSION, compareVersions, observeServerVersion, resetServerVersion, serverIsNewer } from "./version";
import { json, renderWithProviders, signInAs } from "../test-utils";

afterEach(() => {
  vi.unstubAllGlobals();
  resetServerVersion();
});

describe("compareVersions", () => {
  // The same table as pkg/client's TestCompareVersions.
  const cases: [string, string, number][] = [
    ["1.0.0", "1.0.0", 0],
    ["1.0.1", "1.0.0", 1],
    ["1.1.0", "1.0.9", 1],
    ["2.0.0", "1.99.99", 1],
    ["1.10.0", "1.9.0", 1],
    ["1.0.0", "1.0.1", -1],
    ["v1.2.0", "1.2.0", 0],
    ["1.2.0+build5", "1.2.0+build9", 0],
    ["1.0.0-alpha", "1.0.0", -1],
    ["1.0.0", "1.0.0-rc.1", 1],
    ["1.0.0-alpha", "1.0.0-alpha.1", -1],
    ["1.0.0-alpha.1", "1.0.0-alpha.beta", -1],
    ["1.0.0-alpha.beta", "1.0.0-beta", -1],
    ["1.0.0-beta.2", "1.0.0-beta.11", -1],
    ["1.0.0-beta.11", "1.0.0-rc.1", -1],
    ["1.1.0-rc.1", "1.0.0", 1],
    ["", "1.0.0", 0],
    ["1.0.0", "", 0],
    ["1.0", "1.0.0", 0],
    ["1.0.0.0", "1.0.0", 0],
    ["1.x.0", "1.0.0", 0],
    ["01.0.0", "1.0.0", 0],
    ["-1.0.0", "1.0.0", 0],
    ["1.0.0-", "1.0.0", 0],
    ["1.0.0-a..b", "1.0.0", 0],
  ];
  test.each(cases)("%s vs %s is %i", (a, b, want) => {
    expect(compareVersions(a, b)).toBe(want);
    if (want !== 0) expect(compareVersions(b, a)).toBe(-want);
  });

  test("the built version is the contract's", () => {
    expect(BUILT_API_VERSION).toMatch(/^\d+\.\d+\.\d+$/);
    expect(serverIsNewer(BUILT_API_VERSION)).toBe(false);
    expect(serverIsNewer(null)).toBe(false);
    expect(serverIsNewer("banana")).toBe(false);
    expect(serverIsNewer("99.0.0")).toBe(true);
  });
});

describe("the newer-server notice", () => {
  async function renderLayout() {
    const store = new SessionStore();
    await signInAs(store);
    return renderWithProviders(<Layout />, { store });
  }

  test("appears when a response carries a newer version, and not for the same or none", async () => {
    vi.stubGlobal("fetch", async () => json(200, { tenant_id: "t", user_id: "u", role: "member" }, { "X-Taskify-API-Version": BUILT_API_VERSION }));
    await renderLayout();
    await api.GET("/api/v1/me");
    expect(screen.queryByRole("status")).toBeNull();

    vi.stubGlobal("fetch", async () => json(200, { tenant_id: "t", user_id: "u", role: "member" }));
    await api.GET("/api/v1/me");
    expect(screen.queryByRole("status")).toBeNull();

    vi.stubGlobal("fetch", async () => json(200, { tenant_id: "t", user_id: "u", role: "member" }, { "X-Taskify-API-Version": "99.1.0" }));
    await act(async () => {
      await api.GET("/api/v1/me");
    });
    expect(await screen.findByRole("status")).toHaveTextContent("The server is newer than this app (API 99.1.0");
    // The page still works: the navigation is there.
    expect(screen.getByRole("navigation", { name: "Main" })).toBeInTheDocument();
  });

  test("keeps the newest version it saw", () => {
    observeServerVersion("99.1.0");
    observeServerVersion("99.0.0");
    observeServerVersion(null);
    render(<Probe />);
    expect(screen.getByTestId("v")).toHaveTextContent("99.1.0");
  });
});

import { useNewerServerVersion } from "./version";
function Probe() {
  return <span data-testid="v">{useNewerServerVersion()}</span>;
}
