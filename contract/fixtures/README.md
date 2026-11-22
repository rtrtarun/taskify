# Conformance fixtures

Declarative checks of the contract, run by the black-box runner in `tests/conformance` against any server at `TARGET_URL` (docs/architecture.md section 9.3). Each fixture states requests, the expected status, headers, and body, the outbox rows the requests must write, and what AgentPulse must receive. The rules they encode are in [../RULES.md](../RULES.md).

## Running

The runner starts its own mock AgentPulse (`tests/mockagentpulse`) and mock LLM (`tests/mockllm`), so the target must be started pointing at them. It authenticates with development tokens: HS256 JWTs signed with the target's `JWT_SECRET`, as `cmd/devtoken` makes them. It needs no database access and imports no server package; outbox effects are read through `GET /api/v1/admin/outbox`.

Start the target (from the repository root, Git Bash or any POSIX shell), against an empty, migrated database:

```bash
ENV=production PORT=8091 \
DATABASE_URL='postgres://taskify:taskify@localhost:5433/taskify_conformance?sslmode=disable' \
PUBLIC_BASE_URL=http://127.0.0.1:8091 \
AGENTPULSE_BASE_URL=http://127.0.0.1:9190 AGENTPULSE_API_KEY=conformance-key \
AGENTPULSE_WEBHOOK_SECRET=conformance-webhook-secret JWT_SECRET=conformance-jwt-secret \
AGENTS_ENABLED=true LLM_BASE_URL=http://127.0.0.1:9191/v1 LLM_MODEL=mock LLM_TIMEOUT=5s \
OUTBOX_POLL_INTERVAL=50ms OUTBOX_RETRY_MAX_DELAY=100ms SCHEDULER_INTERVAL=1s \
RATE_LIMIT_RPS=1000 RATE_LIMIT_BURST=1000 \
go run ./cmd/taskify
```

Then run the suite:

```bash
TARGET_URL=http://127.0.0.1:8091 go test -v ./tests/conformance/...
```

| Runner variable | Default | Must equal on the target |
| --- | --- | --- |
| `TARGET_URL` | none; without it the suite is skipped | |
| `CONFORMANCE_JWT_SECRET` | `conformance-jwt-secret` | `JWT_SECRET` |
| `CONFORMANCE_WEBHOOK_SECRET` | `conformance-webhook-secret` | `AGENTPULSE_WEBHOOK_SECRET` |
| `MOCK_AGENTPULSE_ADDR` | `127.0.0.1:9190` | `AGENTPULSE_BASE_URL` (`http://` and this address) |
| `MOCK_LLM_ADDR` | `127.0.0.1:9191` | `LLM_BASE_URL` (`http://`, this address, `/v1`) |

Why the target settings matter:

- `ENV=production`: `auth/headers-refused.json` checks that the development identity headers are refused, which a development server would accept.
- `AGENTS_ENABLED=true`, `LLM_*`: the agent and approval fixtures script the mock LLM. Each fixture turns agents on only in its own workspace, so nothing else calls the model.
- `OUTBOX_RETRY_MAX_DELAY=100ms`: `delivery/dead-letter.json` waits for ten failed sends, which take minutes with the default 60-second ceiling.
- `SCHEDULER_INTERVAL=1s`: `events/due-soon-and-overdue.json` waits for the scheduler's reminders.
- `RATE_LIMIT_*`: the runner polls; it also waits and retries on `429`.

Fixtures run one at a time, in path order, each in a new tenant (`conf-` and ten hex digits), so they can run again and again against the same database. `go test -run 'TestConformance/hitl/'` runs one group.

The tests in `tests/conformance` that need no target check the fixtures themselves: they parse, cite rules that exist, every cell of the transition matrix has a fixture, and every numbered rule in RULES.md cites a fixture that exists.

## Format

One JSON object per file, read strictly: an unknown field is an error.

```json
{
  "description": "What the fixture shows, in one sentence or two.",
  "rules": ["R4.1"],
  "workspace": {"agent_settings": {"agents_enabled": true}},
  "steps": [
    {
      "name": "create",
      "as": "member",
      "mock": {"agentpulse_fail": [{"method": "POST", "path": "/v1/notify", "status": 503, "count": 1}],
               "llm": [{"match": "{{task}}", "answers": [{"content": "Done."}]}]},
      "request": {"method": "POST", "path": "/api/v1/tasks", "headers": {"If-Match": "\"1\""}, "body": {"title": "x"}},
      "expect": {"status": 201, "headers": {"ETag": "\"1\""}, "body": {"id": "$uuid"}, "error_code": "not_found"},
      "eventually": {"timeout_seconds": 30},
      "capture": {"task": "id"},
      "outbox": {"rows": [{"event_type": "task.created", "recipient": "usr_carol", "idempotency_key": "...", "status": "dispatched", "attempts": 0, "capture": {"row": "id"}}],
                 "includes": false, "timeout_seconds": 20},
      "agentpulse": {"notifications": [{"eventType": "task.created"}], "notify_calls": 1, "feedback": [], "timeout_seconds": 20}
    }
  ]
}
```

Fixture fields:

| Field | Meaning |
| --- | --- |
| `description` | Required. |
| `rules` | The RULES.md rules the fixture encodes, such as `R4.1`. |
| `workspace` | The runner creates workspace `ws_conf` as an admin before the first step. `{"skip": true}` leaves the tenant without one; `agent_settings` is applied after creation. |
| `steps` | Run in order; the first failing step ends the fixture. |

Step fields:

| Field | Meaning |
| --- | --- |
| `as` | Who sends the request: `member` (default, `usr_bob`), `admin` (`usr_admin`), `other_tenant` (`usr_eve`, admin of another tenant), `none`, `expired`, `bad_signature`, `malformed`, `headers` (the identity headers instead of a token), or `{"user", "role", "tenant"}`. |
| `request` | `method`, `path`, `headers`, and a JSON `body`, or `raw_body` for text that is not JSON. |
| `webhook` | Instead of `request`: an AgentPulse callback the runner signs. `body`; `timestamp_offset_seconds`; `signature` `valid` (default), `wrong`, `malformed`, or `missing`; or `replay_previous: true` to resend the last callback byte for byte. |
| `mock` | Set before the request: `agentpulse_fail` makes the mock AgentPulse fail the next calls to a path; `llm` adds mock LLM scripts (`match`, `answers`, `repeat`), each answer `content` or `tool_calls` (`name` with `arguments`, or `raw_arguments` text). |
| `expect` | Required. `status`; `headers` compared exactly; `body` as a pattern; `error_code` as a shorthand for `error.code`. Every response is also validated against openapi.yaml, whatever the fixture says. |
| `eventually` | Repeat the request until `expect` holds, for background work such as agent runs. |
| `capture` | Store response values for later steps: `{"name": "path.to.value"}`; list items by index, as `entries.0.id`. |
| `outbox` | The rows the step wrote in the tenant, read through `GET /api/v1/admin/outbox`: exactly these, in any order (or at least these with `includes`), waited for up to the timeout. `recipient` is the payload's `userId`. A row's `capture` stores its fields. |
| `agentpulse` | What the mock AgentPulse received for the tenant since the step began: accepted `notifications` (exactly these patterns), `notify_calls` (every `/v1/notify` call, failed ones included), and `feedback` (exactly these patterns). |

Before each step the runner waits for the tenant's outbox to have nothing pending or being sent, so what a step causes is not confused with what earlier steps caused. After the last step it validates every `/v1/notify` body the fixture caused against `contract/events`.

Patterns: an object matches when each of its keys matches (other keys are ignored). A list matches item by item and must have the same length. `"$absent"` requires a key to be missing; `"$any"`, `"$uuid"`, `"$timestamp"` (RFC 3339 UTC), and `"$nonempty"` match by kind; `{"$length": n}`, `{"$contains": [...]}`, and `{"$one_of": [...]}` do what they say.

Variables: `{{name}}` in any string. A string that is a single placeholder takes the value's type, so `"{{version}}"` can be a number. Built in: `tenant`, `other_tenant`, `workspace`, `random_uuid`, and the times `now`, `in_1h`, `in_2h`, `in_30h`, `in_48h`, `ago_1h`, `ago_26h`, each also as `_unix` (seconds) and `_date` (`YYYY-MM-DD`). Captures add to them.
