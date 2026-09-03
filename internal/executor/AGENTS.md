# AGENTS.md — internal/executor

Docker-backed executor that runs Renovate as **containers on the local Docker/Podman daemon** — this is the standalone alternative to Kubernetes Jobs. Only real "moving parts" package: talks to the Docker daemon, dispatches jobs, and reaps exits.

## Docker client
- Uses `github.com/docker/docker/client` with `client.FromEnv` + `WithAPIVersionNegotiation()` — respects `DOCKER_HOST`, `DOCKER_TLS_VERIFY`, etc. Works against Podman's Docker-compatible socket too (log demux is handled for both, see `isDockerMultiplexed`).
- One client per `DockerExecutor`. `Stop()` closes it.

## Container lifecycle
- Two container types, distinguished by the label `renovate-standalone/type` = `executor` or `discovery`.
- Every container is labeled with: `renovate-standalone/project`, `renovate-standalone/job-name`, `renovate-standalone/type`, `renovate-standalone/started-at` (RFC3339). Never remove/rename these — orphan reconciliation and `handleContainerExit` depend on them.
- Container name pattern: `renovate-<sanitized-project>-<unix>` / `renovate-discovery-<sanitized-job>-<unix>`. `sanitizeName` lowercases and replaces `/ . : space` with `-`, truncates to 60 chars.
- User: hardcoded `12021:12021` (the renovate/renovate image's `ubuntu` user).
- Bind mount: single volume `<cacheVolume>:/tmp/renovate` (default `renovate-cache`). No per-project isolation — Renovate manages its own subdirs.
- Container removal is **always forced** (`Force: true`) after wait, on every error path. Cleanup is idempotent.

## Event loop
`Start()` spawns 3 goroutines that must all be running:
1. `listenEvents` — subscribes to Docker events filtered to `type=container, event=die, label=renovate-standalone/type`. Auto-reconnects with 5s backoff on stream errors. Exit handlers run in a bounded pool of size `max(parallelism*2, 8)`.
2. `dispatchLoop` — polls every 10s **and** on-demand via a buffered signal channel (`triggerDispatch` after each exit).
3. `reconcileLoop` — every 60s scans containers with our labels not tracked in `running`. Re-adopts running ones under `jobTimeout`, kills timed-out orphans, replays exit handling for stopped/dead orphans. This is the recovery path after a restart.

## Loop supervision (fatal-on-early-exit)
Each of the three `Start()` goroutines opens with `defer supervise.Guard(ctx, e.logger, "<name>")()`. `Guard` panics-and-exits the process (via `log.Fatal`) if the guarded function returns while the parent `ctx` is still live, or if a panic is unwinding. This enforces the cross-cutting invariant that background loops never silently die.

**Shutdown-order dependency:** clean shutdown must cancel the executor's parent `ctx` **before** `Stop()` closes `stopCh`, otherwise the loops return on `stopCh` with `ctx.Err() == nil` and the guard treats it as an unexpected exit. `cmd/main.go` orders it correctly today (`ctxCancel()` then `exec.Stop()`). If you refactor shutdown, preserve that ordering — or move the `close(stopCh)` inside `Stop()` after an internal `context.WithCancel` on the constructor-supplied ctx.

## Discovery is special
- Sync path: `DispatchDiscovery` blocks until the container exits. Used by `scheduler` on cron ticks.
- Async path: `DispatchDiscoveryAsync` returns immediately after starting; tracks the container ID in `discoveryID[jobName]` for cancellation. Returns `ErrDiscoveryAlreadyRunning` if a discovery is already in flight for that job. Used by the HTTP `/api/v1/discovery/start` endpoint.
- Discovery command is a shell one-liner that runs `renovate --autodiscover --write-discovered-repos "$BASE_DIR/repos.json"`, then `cat`s either the repos JSON (on success) **or** the log file (on failure) to stdout. Parsing expects a JSON array; if that fails, the raw stdout is embedded in the error.

## Env var construction (`buildEnvVars`)
Priority chain (lowest → highest):
1. Overridable defaults (`RENOVATE_LOG_FORMAT=json`, `NODE_NO_WARNINGS=1`, `RENOVATE_BASE_DIR=/tmp/renovate`, `RENOVATE_PLATFORM/ENDPOINT` from job spec).
2. `RENOVATE_LOG_LEVEL` — default `debug` (needed for PR-activity parsing); flipped to `info` only when `executionOptions.Debug == false`.
3. Discovery-only: `RENOVATE_AUTODISCOVER_FILTER`, `RENOVATE_AUTODISCOVER_TOPICS`.
4. Passthrough: every `RENOVATE_*` env var from the operator process (**overrides** everything above).
5. Protected re-enforcement: `RENOVATE_LOG_FORMAT` and `RENOVATE_BASE_DIR` are re-clamped after passthrough — a warning is logged if the passthrough tried to override them.
6. Per-job `spec.extraEnv` — **highest priority**, wins over everything including passthrough.

Result is sorted for deterministic container specs.

## Log handling gotchas
- `getContainerLogs` auto-detects Docker's 8-byte multiplex framing (`isDockerMultiplexed`) so Podman's raw log stream works too. Never assume `stdcopy.StdCopy` is always needed.
- `StreamContainerLogs` (live) demuxes with `stdcopy.StdCopy` and wraps any non-JSON stderr lines as NDJSON on the fly (`stderrJSONWrapper`) so the UI can always parse the stream as NDJSON.
- Logs are stored in the statestore **before** being parsed. Parse failure never blocks storing raw logs.

## Outcome classification
`classifyOutcome(exitCode, runtime)`:
- exit 0 → `OutcomeSuccess`.
- exit ≠ 0, runtime < `FailureMinRuntime` (default 30s) → `OutcomeRapidFail` (feeds the circuit breaker).
- exit ≠ 0, runtime ≥ `FailureMinRuntime` → `OutcomeSlowFail`.
Every exit (including discovery) reports to `resilience.Manager` and `metrics.Recorder` via nil-safe interfaces (`ResilienceReporter`, `MetricsRecorder`) — those are wired via `SetReporter`/`SetRecorder` after construction, so a fresh executor is safe to use without them.

## Dispatch loop invariants
- `running[project]` acts as a **reservation lock** — set to empty string during dispatch, filled with the container ID after `ContainerStart` succeeds. If start fails, the entry is deleted (releases the slot). Never read `running[proj]` and rely on non-empty container ID for "is running".
- Sources are stored in `sources[project]` before dispatch (default `SourceCron`); the HTTP handlers overwrite this via `SetProjectSource` to attribute manual/webhook triggers before the exit handler consumes it.
- Parallelism cap is enforced only against `len(e.running)`, **not** against the state store's Running count — restart re-adoption fills `running` via `reconcileOrphans`.

## Image pull policy
- `always` — always pulls. `never` — never pulls. `if-not-present` (default) or any unknown value — inspects local image, pulls if absent.
- Pull results are cached in `imageCache[image]=lastCheckedTime` when `ImageCacheTTL > 0`, so repeat inspections are skipped within the TTL.

## Test gotchas
- `docker_test.go` has **no build tags** and **no `t.Skip`** — it tests pure functions only (`sanitizeName`, `classifyOutcome`, `isDockerMultiplexed`, `buildEnvVars`). Do **not** add tests that require a real Docker daemon here without adding a build tag or skip check.
- `buildEnvVars` reads `os.Environ()` — tests that assert env output must `t.Setenv` any `RENOVATE_*` they care about, and be aware that ambient `RENOVATE_*` vars from the shell will leak in.

## What NOT to do
- Do not add labels outside the `renovate-standalone/` namespace — orphan filters won't see them.
- Do not remove containers before calling `ContainerInspect` in `handleContainerExit` — exit code and labels are read from the inspection.
- Do not call `SetProjectSource` after dispatch — the exit handler may have already consumed it.
