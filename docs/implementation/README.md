# Taskify Implementation Plan

This folder breaks the open milestones in [milestones.md](../milestones.md) into tasks small enough to hand to one implementer (a person or a coding model) at a time. It contains no code.

| Document | Answers |
| --- | --- |
| [features.md](../features.md) | What we are building and why |
| [architecture.md](../architecture.md) | How it is designed. This is the authority for every task below. |
| [milestones.md](../milestones.md) | Delivery order, deliverables, and exit criteria per milestone |
| This folder | Who builds which piece, in what order, and how we know it is finished |
| [releasing.md](../releasing.md) | How merges to `main` are versioned, packaged, and released |

Open work:

- [r3-clients.md](r3-clients.md): M14-13 and M15-01 to M15-05, 6 tasks.
- M0-07, the check against a real AgentPulse: section 2 below.

Finished task lists, the record of what was built, and superseded documents are in [docs/archive](../archive/README.md).

## 1. How the work is classified

### 1.1 By work stream

| Stream | Milestones | What it contains |
| --- | --- | --- |
| Foundation | M0 | Build, tooling, CI, mock AgentPulse, contract check |
| Data and domain | M1, M5 (schema part) | Migrations, Go types, state machine, events, keys |
| Persistence and delivery | M2 | Repositories, `TaskService`, outbox relay, scheduler, AgentPulse client |
| HTTP surface | M3, M6 (endpoints), M12 (list endpoints) | Router, auth, handlers, webhook, admin proxies |
| Operations | M4 | Health, metrics, tracing, shutdown, benchmarks |
| Agent engine | M5, M6 | Job queue, LLM client, tool registry, runtime, approval flow |
| Agents | M7 to M11 | Triage, decomposer, blocker, digest, learning |
| Contract | M12 | OpenAPI, schemas, fixtures, conformance runner |
| Clients | M13, M14, M15 | CLI, web frontend, Python SDK |

### 1.2 By who should do it

Every task carries one of three levels.

| Level | Meaning | Share |
| --- | --- | --- |
| **Routine** | Well specified, low risk. Hand to a smaller model or a junior engineer and review the diff normally. | 81 tasks |
| **Careful** | Concurrency, security, or transactional correctness. Can still be delegated, but the result needs review by a stronger model or a senior engineer before merge, and its tests must run with `-race`. | 24 tasks |
| **Owner** | Needs access, a decision, or an account only the project owner has. Cannot be delegated. | Part of 3 of the tasks above (M0-07, M14-03, M15-05), plus the confirmations in section 3 |

The Careful tasks are where a wrong implementation still passes a casual test: the outbox claim and relay, the scheduler, dependency cycle checks, the agent runtime, proposal apply, authentication, the webhook, and permanent delete.

## 2. Owner actions

These block specific tasks. Everything else can proceed without them.

| # | Action | Blocks | Until then |
| --- | --- | --- | --- |
| O1 | Resolved. The real AgentPulse was read on 2026-11-22; its contract did not match section 5.5, and the owner chose to extend AgentPulse to Taskify's design (option B, decision 27). The agreed contract is in [blockers.md](../blockers.md). Remaining: task M0-07 runs the client and webhook tests against a real AgentPulse when AgentPulse slices 40a to 40d ship | Final sign-off of M2, M3, M10, M11, M14 notification feed | Work proceeds against the mock AgentPulse (M0-06), which is the specification AgentPulse must meet. All AgentPulse field names stay inside `internal/agentpulse`, so a correction touches one package. |
| O2 | Deferred by the owner (decision 38). Choose the identity provider that issues JWTs (default G17) | `taskctl login` (M13-02), frontend sign-in (M14-03) | Development uses locally minted HS256 tokens from `cmd/devtoken`, until one is chosen. The web session is held in memory only; a refresh cookie and the provider redirect wait for this (D72). |
| O3 | Reserve `taskify-sdk` on the public Python index (decision 26) | Nothing | Not needed until v1.0. |

The one open task outside R3:

| ID | Task | Files | Done when | Level, size | After |
| --- | --- | --- | --- | --- | --- |
| M0-07 | **AgentPulse contract check.** Read the real request and response shapes from the AgentPulse repository or SDK and write them down. Work through the checklist under M0 in [milestones-completed.md](../archive/milestones-completed.md), plus G7 (signature layout, action names) and G16 (feedback analytics endpoint). Correct section 5.5 and the mock where they differ. | `docs/agentpulse-contract.md`, corrections to `architecture.md` and `tests/mockagentpulse/` | Every field in section 5.5 is marked confirmed or corrected | **Owner** (access, O1), then Careful, M | none |

## 3. Open defaults and gaps

Points the architecture leaves open that still need an owner's confirmation, a run against a real AgentPulse, or a decision. **A default is a proposal by the author of this plan, not an owner decision**, unless its row says otherwise. The defaults already settled are in the [implementation record](../archive/implementation-record.md).

| # | Gap | Default used in the tasks | Worth confirming |
| --- | --- | --- | --- |
| G3 | Admin-only endpoints exist, but where the role comes from is not stated | JWT claim `role` with values `admin` or `member`. In the development header fallback, `X-User-Role`. | Yes |
| G7 | Webhook signature layout and action names are not given | Header `X-AgentPulse-Timestamp`; signature is hex HMAC-SHA256 of `{timestamp}.{raw body}`. Actions: `complete`, `snooze`, `unblock`, `approve`, `reject`. Agreed with AgentPulse (2026-11-22), with `callbackId` for retries, deduplicated on `(tenantId, callbackId)` (AP-06, D47). | Agreed (decision 32); run against a real AgentPulse in M0-07 |
| G8 | What "Snooze 2h" changes in Taskify | Sets `remind_at` to now plus the duration and forwards `snoozed` feedback. No task event. | Yes |
| G10 | Who may approve or reject a proposal | The task's assignee, its creator, or an admin. Anyone else gets `403`. | Yes |
| G16 | The learning agent "reads AgentPulse feedback analytics", but no endpoint is named | Resolved by [blockers.md](../blockers.md) R7: `GET /v1/analytics/feedback?tenantId=&since=`. The job runs weekly: one per tenant per ISO week (D55). | Resolved (R7, D54 to D58); run against a real AgentPulse in M0-07 |
| G17 | No identity provider is named and Taskify has no login endpoint | Owner action O2, deferred (decision 38). Development uses `cmd/devtoken`. | Deferred by the owner |
| G23 | A person adding a dependency is mentioned in architecture section 4.3, but section 5.4 has no endpoint for it | Only the service operation exists until an endpoint is specified (rule 12: not built ad hoc). | Needs a decision |

## 4. Order of work

M14-13 and M15 are independent of each other. M0-07 runs when AgentPulse slices 40a to 40d ship; it blocks nothing else.

One milestone is finished only when every exit criterion in [milestones.md](../milestones.md) passes, not when its task list is ticked off.

## 5. How to hand a task to an implementer

Give the implementer one task ID at a time, or a few tasks from the same milestone that touch the same package.

Brief template:

```text
Implement task <ID> from docs/implementation/<file>.md in the taskify repository.

Read first:
- the task row, and the rows it lists under After
- the sections of docs/architecture.md named in the task
- docs/implementation/README.md sections 3 and 6

Stay inside the files the task names. Write the tests the task lists.
Finish by running: go build ./... && go vet ./... && go test -race ./...
Report: what you built, test output, and anything in the architecture that
was unclear or contradictory. Do not resolve a contradiction by guessing.
```

For a Careful task, add: "Explain how the code behaves when two replicas run this at the same time."

## 6. Rules for every task

1. **The architecture wins.** If a task row and [architecture.md](../architecture.md) disagree, follow the architecture and report the disagreement.
2. **No remote calls on a task write path.** Handlers that change tasks write to PostgreSQL only. The few endpoints that exist to talk to AgentPulse use a timeout and the circuit breaker.
3. **One transaction per state change.** Task row, outbox rows, and agent job rows commit together.
4. **Tenant comes from the request context**, never from a request body or from model output. Every query filters on it. Another tenant's row is `404`.
5. **Dependency direction:** `api` → `service` → `domain`. `domain` imports nothing from the project.
6. **Tests come with the code.** Table-driven where there is a matrix. `go test -race ./...` passes before a task is reported as done. Tests that need PostgreSQL are skipped with a clear message when `DATABASE_URL` is unset.
7. **Migrations have a working down script.**
8. **Secrets are never logged**, including in tests and error messages.
9. **AgentPulse payloads are camelCase; the Taskify API is snake_case.**
10. **Commit messages follow Conventional Commits** (`feat:`, `fix:`, `docs:`, ...): they decide the next release version. See [releasing.md](../releasing.md). Commit messages carry no `Co-Authored-By` trailer.
11. **From M12 onward**, a change to an endpoint, event, or tool is made in `contract/` first, and generated clients are regenerated in the same change.
12. **Do not widen scope.** A missing prerequisite is reported, not built ad hoc.

## 7. Progress

Update this table as milestones close. M1 to M13 are done; see the [implementation record](../archive/implementation-record.md).

| Milestone | Tasks | Status |
| --- | --- | --- |
| M0 Foundation | 7 | Done except M0-07, which runs against a real AgentPulse when its slices 40a to 40d ship (O1) |
| M14 Web frontend (R3) | 13 | In progress. M14-01 to M14-12 are done. M14-13 (journeys, accessibility, small screens) remains |
| M15 Python SDK | 5 | Not started |

## 8. Departures found during implementation

Recorded as they come up, so the architecture can be corrected or the code changed. D1 to D87 are in the [implementation record](../archive/implementation-record.md#departures-found-during-implementation); number new ones from D88.

| # | Where | What the code does | Why |
| --- | --- | --- | --- |
