import { ApiError, unwrap } from "./client";

function res(status: number, headers: Record<string, string> = {}): Response {
  return new Response(null, { status, headers });
}

describe("unwrap", () => {
  test("returns data on success", () => {
    expect(unwrap({ data: { ok: 1 }, response: res(200) })).toEqual({ ok: 1 });
  });

  test("throws an ApiError carrying the code and retry hint", () => {
    const body = { error: { code: "upstream_unavailable", message: "later" } };
    try {
      unwrap({ error: body, response: res(503, { "Retry-After": "7" }) });
      expect.unreachable();
    } catch (e) {
      expect(e).toBeInstanceOf(ApiError);
      const err = e as ApiError;
      expect([err.status, err.code, err.retryAfter]).toEqual([503, "upstream_unavailable", 7]);
    }
  });
});
