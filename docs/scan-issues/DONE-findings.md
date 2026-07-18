# Issue Scan Results

> Generated: 2026-07-17 | Scope: Full codebase scan | Status: Investigation only (no fixes applied)

---

## Critical (P0)

| # | Finding | Location | Description |
| --- | --------- | ---------- | ------------- |
| 1 | **No authentication on API endpoints** | `internal/server/server.go:73-90` | All API routes (`/api/v1/renovate`, `/renovate/all`, `/renovate/cancel`, `/discovery/start`, `/executionOptions`) have zero auth. Anyone with network access can trigger jobs, cancel projects, run discovery, or toggle debug mode. OIDC config exists in config but is never wired into the server. |
| 2 | **ExtraEnv can override security-sensitive vars** | `internal/executor/docker.go:698` | User-supplied `ExtraEnv` from the job spec is applied *after* `RENOVATE_TOKEN` and `RENOVATE_ENDPOINT`. A malicious/misconfigured job can override the token, redirecting Renovate to an attacker-controlled endpoint. No deny-list is enforced. |
| 3 | **Unbounded request body reads (DoS)** | `internal/webhook/forgejo.go:85,159` + `internal/server/server.go:154,171,191,228` | `io.ReadAll(r.Body)` and `json.NewDecoder(r.Body).Decode()` are used without `http.MaxBytesReader`. An attacker can exhaust memory with a multi-GB payload. |

## High (P1)

| # | Finding | Location | Description |
| --- | --------- | ---------- | ------------- |
| 4 | **Race condition in dispatch loop** | `internal/executor/docker.go:276-290` | `runningCount` is read under mutex, but dispatching happens without the lock. Between check and dispatch, other goroutines can change state, potentially exceeding the parallelism limit or dispatching duplicates. |
| 5 | **Webhook tokens not per-job** | `internal/statestore/sqlite.go:448-451` | `IsWebhookTokenValid` and `IsWebhookSignatureValid` compare against a global token list regardless of which `RenovateJobIdentifier` is passed. A valid token for job A authenticates requests for job B, defeating per-job isolation. |
| 6 | **Unbounded goroutine spawning on container exits** | `internal/executor/docker.go:440` | Every "die" event spawns a goroutine with no backpressure. Under heavy churn, this causes goroutine explosion. |
| 7 | **containerLogs: unbounded io.ReadAll** | `internal/containerLogs/docker_logs.go:40` | `GetLogs` uses `io.ReadAll` without limit. A container producing GBs of output will OOM the operator. |
| 8 | **Context.Background() never cancelled** | `cmd/main.go:76` | The context passed to `exec.Start(ctx)` and `runScheduledCycle` is never cancelled on shutdown. If `Stop()` fails, goroutines leak indefinitely. |
| 9 | **Docker client never closed** | `internal/executor/docker.go` | `DockerExecutor` creates a `client.Client` but `Stop()` never calls `e.docker.Close()`. |
| 10 | **containerLogs: raw multiplexed stream not demuxed** | `internal/containerLogs/docker_logs.go:27-33` | `StreamLogs` / `GetLogs` returns raw Docker multiplexed stream without `stdcopy.StdCopy`. Callers get garbled output with 8-byte frame headers mixed in. *(Note: `executor/docker.go` fixed this in `getContainerLogs`, but `containerLogs` package still has the bug.)* |

## Medium (P2)

| # | Finding | Location | Description |
| --- | --------- | ---------- | ------------- |
| 11 | **No CORS/CSRF protection** | `internal/server/server.go:88` | State-mutating POST endpoints have no CSRF tokens. A malicious page could trigger job actions via the user's browser. |
| 12 | **Missing RENOVATE_TOKEN validation** | `config/config.go:93-98` | The token is required for operation but not validated at startup. The operator starts silently without it and every container then fails. |
| 13 | **Discovery container double-shell wrapping** | `internal/executor/docker.go:192` | `Cmd: []string{"/bin/sh", "-c", discoveryCmd}` but `discoveryCmd` already starts with `/bin/sh -c '...'`. This creates `sh -c "sh -c '...'"` which is fragile. |
| 14 | **Logs fetched then discarded** | `internal/executor/docker.go:510` | `logs, err := e.getContainerLogs(...)` followed by `_ = logs`. Dead code that wastes a Docker API call and memory allocation. |
| 15 | **Authentication bypass via candidate iteration** | `internal/webhook/forgejo.go:168-182` | `findAndAuthenticateJob` returns the first job with auth disabled. If multiple jobs match a project and one has auth disabled, it's returned without credential checks. |
| 16 | **Scheduler Stop() blocks indefinitely** | `internal/scheduler/scheduler.go:44-46` | `<-ctx.Done()` waits for all cron jobs forever. If a discovery cycle hangs, the operator never shuts down. Should add timeout. |
| 17 | **pullImage called on every dispatch** | `internal/executor/docker.go:343` | Even with `if-not-present`, each dispatch does a Docker API `ImageInspect`. Under load, this is N API calls per cycle. Should cache with TTL. |
| 18 | **Events reconnect with cancelled context** | `internal/executor/docker.go:365` | If context was cancelled (causing the error), reconnecting with the same ctx creates an infinite fail loop burning CPU. |
| 19 | **Container start not atomic with state tracking** | `internal/executor/docker.go:318-344` | Container starts, then state updates, then tracking. Crash between start and tracking creates orphans (handled by reconcile, but 60s gap). |
| 20 | **CancelProjectJob read-then-write race** | `internal/statestore/sqlite.go:503` | Reads container ID from `readDB`, then updates via `writeDB` without a transaction. State can change between the two operations. |
| 21 | **Silenced JSON unmarshal errors** | `internal/statestore/sqlite.go:617-633` | Corrupted JSON in DB is silently set to nil instead of surfacing errors. Makes data corruption very hard to debug. |

## Low (P3)

| # | Finding | Location | Description |
| --- | --------- | ---------- | ------------- |
| 22 | **Unused parameter in buildEnvVars** | `internal/executor/docker.go:671` | `_ string` (project name) is always ignored — dead parameter. |
| 23 | **SSE framing for static blob** | `internal/server/server.go:218` | Log streaming uses SSE but underlying data is a static SQLite blob. No streaming benefit — adds complexity for nothing. |
| 24 | **SessionSecret never auto-generated** | `config/config.go:47` | Comment says "auto-generated if empty" but no generation logic exists. If not set, sessions use empty key. |
| 25 | **Reconcile re-adopts without state store sync** | `internal/executor/docker.go:471-493` | Re-adopted orphans aren't reflected in state store, causing status inconsistency. |
| 26 | **Start() returns error but never fails** | `internal/executor/docker.go:135` | Spawns goroutines and always returns nil. Goroutine startup failures are only logged. |
| 27 | **No type validation on api.RenovateJobSpec** | `internal/api/types.go` | No validation methods — schedule can be empty/invalid, parallelism can be negative. |
| 28 | **configValues global written unsynchronized** | `config/config.go:85` | Package-level map written in `Load()` without sync. Data race if called concurrently with `GetValue()`. Unlikely but latent. |
| 29 | **Secrets in package-level global map** | `config/config.go:105-118` | All secrets (tokens, session secret) stored in plaintext in a global map accessible by any package. Increases blast radius of memory dumps. |

---

## Summary

| Severity | Count |
| ---------- | ------- |
| **Critical (P0)** | 3 |
| **High (P1)** | 7 |
| **Medium (P2)** | 11 |
| **Low (P3)** | 8 |
| **Total** | **29** |

---

## Top Recommendations

### Immediate (Critical)

1. **Add authentication middleware to API routes** — wire the existing OIDC config or require a shared bearer token.
2. **Add `http.MaxBytesReader`** (e.g., 1MB) on all request body reads.
3. **Add a deny-list for `ExtraEnv`** preventing override of `RENOVATE_TOKEN`, `RENOVATE_ENDPOINT`, `RENOVATE_PLATFORM`.

### Short-term (High)

1. Fix the dispatch loop race condition — hold the mutex across the entire dispatch decision or use a semaphore channel.
2. Add bounded worker pool for container exit handling.
3. Fix `containerLogs.GetLogs()` to use `stdcopy.StdCopy` (like `executor.getContainerLogs` already does) and add a size limit.
4. Use a cancellable context for executor goroutines.

---

> **Critical issues found. Recommend addressing immediately.**
>
> Suggested next steps:
>
> ```
> /unipi:brainstorm "security hardening plan — auth middleware, request limits, env var protection"
> ```
>
> or for the highest priority fix:
>
> ```
> /unipi:quick-work "add http.MaxBytesReader to all webhook/API handlers and deny-list ExtraEnv overrides"
> ```
