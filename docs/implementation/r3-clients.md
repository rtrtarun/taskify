# R3 Tasks: Clients (M14 and M15)

Read [README.md](README.md) first: it explains the levels (Routine, Careful, Owner) and the rules every task follows. Section numbers below refer to [architecture.md](../architecture.md), mostly section 9. Exit criteria for each milestone are in [milestones.md](../milestones.md).

This file holds the open tasks only. M12, M13, M14a, M14b, M14-11, and M14-12 are done: [archive/r3-completed.md](../archive/r3-completed.md).

Two rules apply to every task in this file:

- Clients contain no business rules: no transition checks, no key building, no tier logic. Those stay in the server.
- Generated code is never edited by hand. CI regenerates it and fails on a difference.

---

## M14. Web frontend

The stack is React, TypeScript, Vite, TanStack Query, and React Router. Task and comment text is always rendered as text, never as HTML, including text written by agents.

### M14c. Admin and delivery (needs M12)

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M14-13 | **Journeys, accessibility, and small screens.** The end-to-end journeys listed under M14 in [milestones.md](../milestones.md); keyboard navigation and labelled controls on the core flows; list, detail, and approvals usable at phone width; a cross-tenant task URL does not load. | `web/e2e/` | All journeys pass in CI | Routine, M | M14-09, M14-11 |

---

## M15. Python client SDK

Goal: Python scripts, pipelines, and agent workflows use Taskify through a typed client (feature C2). Design in section 9.4.

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M15-01 | **Package and generated models.** `pyproject.toml` with distribution name `taskify-sdk` and import name `taskify`, Python 3.10 or later. Pydantic models generated from `openapi.yaml` into `_generated/`. A CI check that regeneration produces no difference. | `sdk/python/` | A changed contract without regenerated models fails CI | Routine, S | M12-05 |
| M15-02 | **Client core.** Sync and async clients on `httpx` sharing resource definitions; bearer token passed in, read from `TASKIFY_TOKEN`, or supplied by a callable; one typed exception per error code, with `VersionConflictError` carrying the current version; retries with backoff for `GET` only; SDK version in `User-Agent`; unknown response fields ignored. | `sdk/python/src/taskify/client.py`, `errors.py`, `auth.py` | Tests: each error code maps to its exception; a write is never retried; the token appears in no log line or exception message | Routine, M | M15-01 |
| M15-03 | **Resources.** Tasks, comments, agent jobs, notifications, admin. Cursor-following iterators; `update()` sends the object's `version` as `If-Match`; `decompose()` returns a job with `wait(timeout=...)`. | `sdk/python/src/taskify/resources/`, `pagination.py` | Tests against a local Go server cover create, paginated list, update, conflict, comment, complete, decompose and wait, approve | Routine, M | M15-02; job resources need M6 and M8 |
| M15-04 | **Quality gates.** `ruff`, `mypy`, and `pytest` in CI, with the Go server started for the integration tests. | CI, `sdk/python/tests/` | The SDK job is green | Routine, S | M15-03 |
| M15-05 | **README and distribution.** Usage examples and the Git-tag install command. No public wheels before v1.0 (decision 21). Reserving the name on the public index is owner action O3. | `sdk/python/README.md` | The install command works against a tag | Routine, S; reservation is **Owner** | M15-04 |

R3 is complete when every exit criterion for M12 to M15 in [milestones.md](../milestones.md) passes.
