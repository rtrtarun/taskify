import { useSyncExternalStore } from "react";

/** The header in which the server states the contract version it implements. */
export const API_VERSION_HEADER = "X-Taskify-API-Version";

/** The contract version this app was built from. */
export const BUILT_API_VERSION: string = __API_VERSION__;

interface Semver {
  nums: [number, number, number];
  pre: string[];
}

function parse(input: string): Semver | null {
  let s = input.trim().replace(/^v/, "");
  s = s.split("+")[0];
  const dash = s.indexOf("-");
  const core = dash < 0 ? s : s.slice(0, dash);
  const pre = dash < 0 ? [] : s.slice(dash + 1).split(".");
  const parts = core.split(".");
  if (parts.length !== 3) return null;
  const nums: number[] = [];
  for (const p of parts) {
    if (!/^(0|[1-9]\d*)$/.test(p)) return null;
    nums.push(Number(p));
  }
  if (pre.some((id) => id === "")) return null;
  return { nums: nums as Semver["nums"], pre };
}

const cmp = (a: number, b: number) => (a < b ? -1 : a > b ? 1 : 0);

function cmpIdent(a: string, b: string): number {
  const an = /^\d+$/.test(a);
  const bn = /^\d+$/.test(b);
  if (an && bn) return cmp(Number(a), Number(b));
  if (an) return -1;
  if (bn) return 1;
  return a < b ? -1 : a > b ? 1 : 0;
}

/**
 * compareVersions orders two semantic versions as pkg/client's
 * CompareVersions does (semver.org section 11): numbers as numbers, a
 * pre-release below its release, build metadata and a leading "v" ignored.
 * It returns 0 when either side is not a version.
 */
export function compareVersions(a: string, b: string): number {
  const va = parse(a);
  const vb = parse(b);
  if (!va || !vb) return 0;
  for (let i = 0; i < 3; i++) {
    const c = cmp(va.nums[i], vb.nums[i]);
    if (c !== 0) return c;
  }
  if (va.pre.length === 0 && vb.pre.length === 0) return 0;
  if (va.pre.length === 0) return 1;
  if (vb.pre.length === 0) return -1;
  for (let i = 0; i < Math.min(va.pre.length, vb.pre.length); i++) {
    const c = cmpIdent(va.pre[i], vb.pre[i]);
    if (c !== 0) return c;
  }
  return cmp(va.pre.length, vb.pre.length);
}

/** Whether the server's version is newer than the one this app was built from. */
export function serverIsNewer(serverVersion: string | null, built: string = BUILT_API_VERSION): boolean {
  return serverVersion != null && compareVersions(serverVersion, built) > 0;
}

// The newest server version seen on a response, shared with the layout.
let newer: string | null = null;
const listeners = new Set<() => void>();

/** observeServerVersion records a response's version header. */
export function observeServerVersion(header: string | null): void {
  if (!serverIsNewer(header) || header === newer) return;
  if (newer !== null && compareVersions(header!, newer) <= 0) return;
  newer = header;
  listeners.forEach((l) => l());
}

/** The newer server version seen so far, or null. */
export function useNewerServerVersion(): string | null {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => newer,
  );
}

/** Forgets what was seen. For tests. */
export function resetServerVersion(): void {
  newer = null;
  listeners.forEach((l) => l());
}
