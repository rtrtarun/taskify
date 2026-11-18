# Benchmarks

Latency of the three paths with targets in [architecture.md](architecture.md) section 6.2, measured through the fully assembled service against PostgreSQL 16.

## How to run

```bash
DATABASE_URL=postgres://taskify:taskify@localhost:5433/taskify?sslmode=disable \
  go test -run '^$' -bench . -benchtime 2000x ./tests/bench/
```

Each benchmark creates its own database, so it can run against any server where the role may create databases. It reports P50, P95, and P99 in milliseconds next to Go's mean (`ns/op`).

| Benchmark | Measures |
| --- | --- |
| `BenchmarkWritePath` | `POST /api/v1/tasks`: authentication, one transaction writing the task and its outbox row, the response |
| `BenchmarkReadPath` | `GET /api/v1/tasks?assignee_id=…&status=todo&limit=50` over 5,000 tasks for 50 assignees |
| `BenchmarkDispatch` | From the create request returning (committed) to the mock AgentPulse receiving `/notify`, with the relay running as in production |

Requests are served in-process, without a network hop to the client, so the HTTP figures are handler latency. The database is reached over the network as in production.

## Results

Recorded on 2026-11-18, 2,000 iterations each, on one machine: Intel Core i5-1335U (12 threads), Windows 11, Docker Desktop running PostgreSQL 16.

### Benchmark in a Linux container on the database's Docker network

This is the configuration closest to a deployment: the service and PostgreSQL on the same Linux network.

| Path | Target | P50 | P95 | P99 | Met |
| --- | --- | --- | --- | --- | --- |
| Write | P95 < 15 ms, P99 < 30 ms | 3.0 ms | 6.6 ms | 10.6 ms | Yes |
| Read | P95 < 8 ms | 0.8 ms | 2.0 ms | 2.8 ms | Yes |
| Dispatch | P95 < 150 ms, P99 < 400 ms | 3.4 ms | 8.2 ms | 12.5 ms | Yes |

### Benchmark on the Windows host, database in Docker

| Path | Target | P50 | P95 | P99 | Met |
| --- | --- | --- | --- | --- | --- |
| Write | P95 < 15 ms, P99 < 30 ms | 9.1 ms | 24.9 ms | 38.4 ms | No |
| Read | P95 < 8 ms | 3.3 ms | 7.6 ms | 17.2 ms | Yes, P95 only |
| Dispatch | P95 < 150 ms, P99 < 400 ms | 3.9 ms | 7.6 ms | 12.0 ms | Yes |

**Why the write path misses here.** Every database round trip crosses from Windows into Docker Desktop's Linux VM, which adds about a millisecond each. A task write makes several round trips (begin, workspace check, task insert, outbox insert, commit), so the overhead multiplies; the same code meets the target comfortably when the network hop is local. This configuration is a developer machine, not a deployment shape. After this run, creating a task stopped issuing a watcher delete it never needed, removing one round trip from every create.

**Why dispatch is far below target.** A commit wakes the relay in the same process immediately; the 200 ms poll interval only matters for rows written by another replica, which the benchmark does not exercise. Cross-replica dispatch is bounded by that interval (section 6.2).

## Not measured

- Throughput under concurrent load. These are single-client latencies.
- Cross-replica dispatch latency, where the 200 ms poll dominates.
- A real AgentPulse. The mock answers immediately; a real one adds its own latency to dispatch.
