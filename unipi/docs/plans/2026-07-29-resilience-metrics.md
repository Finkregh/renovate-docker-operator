# Plan: Resilience & Metrics

Spec: `unipi/docs/specs/2026-07-29-resilience-metrics.md`
Branch: `feat/resilience-metrics`
Size: medium (~10 tasks, ~800–1200 LOC incl. tests)

## Guiding principles

- One task = one landable commit. Each task must leave `go build ./...` and
  `go test ./...` green on its own.
- Wire code paths last: the `resilience.Manager` and `metrics` package land
  first as pure libraries with tests, then the executor/scheduler/server hook
  them in.
- No behavioural change until Task 6. Tasks 1–5 add code without altering
  dispatch semantics.

## Task graph

```
T1 (resilience skeleton)
  └─ T2 (backoff logic + tests)
       └─ T3 (breaker window + tests)
            └─ T4 (manual bypass + snapshot API)
T5 (metrics package + tests, independent)
T6 (executor hooks — reports failures/successes)
  needs: T2, T3, T5
T7 (scheduler/dispatcher gate — AllowDispatch)
  needs: T4, T6
T8 (HTTP: /metrics + /api/v1/breaker/reset + /api/v1/status extension)
  needs: T4, T5
T9 (static UI: banner + status pill + reset button)
  needs: T8
T10 (docs + README + integration smoke tests)
  needs: T1..T9
```

Landable ordering: T1 → T2 → T3 → T4 → T5 → T6 → T7 → T8 → T9 → T10.

## Tasks

### T1 — resilience package skeleton

- Files:
  - `internal/resilience/resilience.go` (new)
  - `internal/resilience/resilience_test.go` (new)
- Deliver:
  - `Manager` struct with `sync.Mutex`, injected `clock func() time.Time`.
  - `Config` struct with all knobs from spec §5.1 + defaults.
  - `New(cfg, logger)` constructor; `WithClock(fn)` test hook.
  - `State` enum + stub methods: `Snapshot()`, `AllowDispatch()`, `Report()`,
    `MarkManual()`, `Reset()`. All no-ops that compile.
- Tests: constructor default-fills zero-value `Config`.
- Acceptance: `go build ./... && go test ./internal/resilience/...` green.

### T2 — per-project backoff logic

- Files: `internal/resilience/resilience.go`, `_test.go`.
- Deliver:
  - `Report(project, source, outcome)` where `outcome` is
    `{Success, RapidFail, SlowFail}`.
  - Per-project state map: `consecutiveFailures`, `nextAllowedAt`.
  - Exponential backoff with jitter: `base * 2^(n-1)` capped at `Max`,
    ±20% jitter. Reset on `Success`.
  - `AllowDispatch(project, source) (allowed bool, reason string, retryAfter time.Duration)`
    honours per-project `nextAllowedAt`. Ignores breaker for now.
- Tests (table-driven, injected clock):
  - Rapid fail bumps counter and `nextAllowed`.
  - Success resets both.
  - Backoff caps at Max.
  - Jitter is in ±20% band across 100 samples.
  - Slow fail bumps backoff same as rapid.
- Acceptance: coverage of resilience.go ≥ 85%.

### T3 — global rapid-fail breaker

- Files: same package.
- Deliver:
  - Ring buffer / capped slice tracking rapid-fail timestamps within
    `RapidFailWindow`.
  - Trip when count ≥ `RapidFailThreshold`. `State()` returns `Open`.
  - `AllowDispatch` returns `false, "breaker_open", 0` when open unless
    manual bypass.
  - `Reset()` clears buffer, sets state to `Closed`, resets manual override
    flags.
- Tests:
  - N rapid fails within window trips breaker.
  - Fails outside window do not trip.
  - Reset restores closed state.
  - Slow fails do NOT count toward window.
  - Discovery rapid fails DO count (per spec §5.2).
- Acceptance: `go test ./internal/resilience/... -race` green.

### T4 — manual bypass + webhook replay queue + Snapshot()

- Files: same package.
- Deliver:
  - `MarkManual(project)` sets single-shot flag under mutex.
  - `AllowDispatch(project, _)` — regardless of source — consumes the flag
    FIRST, bypassing both backoff and breaker.
  - `EnqueueWebhookReplay(project) error` — idempotent add to bounded
    (10k cap) `pendingReplay` set. Returns error when full.
  - `AllowDispatch` opportunistically removes `project` from `pendingReplay`
    when it returns `allowed=true` (natural pickup).
  - `Reset(actor) ResetSummary` performs a **full reset** (spec §5.7):
    breaker closed, rapid-fail window emptied, all `consecutiveFailures`
    and `nextAllowedAt` cleared, all `manualOverride` flags dropped,
    `pendingReplay` drained. Returns `ResetSummary{PreviousState,
    ReplayedProjects []string, ClearedBackoffs int}`.
  - `Snapshot()` returns a struct suitable for UI + metrics:
    - `State`, `RapidFailCount`, `WindowSeconds`, `PendingReplayCount`.
    - Per-project map: `{ConsecutiveFailures, NextAllowedAt, ManualPending}`.
    - `LastReset time.Time`, `LastTripped time.Time`.
- Tests:
  - Manual flag consumed on first check.
  - Manual flag bypasses open breaker.
  - `EnqueueWebhookReplay` is idempotent and enforces the 10k cap.
  - `Reset` returns the drained replay list and clears everything (breaker,
    per-project counters, manual flags, replay queue) in one call.
  - Opportunistic drain: successful `AllowDispatch` removes from replay set.
  - Snapshot is a deep copy (mutations don't leak).
- Acceptance: package feature-complete.

**Explicit non-goal (spec §5.9):** no startup grace period. The Manager
counts rapid failures from t=0. No implementation work needed — just a
regression test that asserts this behavior stays intact.

### T5 — metrics package

- Files:
  - `internal/metrics/metrics.go` (new)
  - `internal/metrics/http.go` (new — middleware for HTTP duration)
  - `internal/metrics/metrics_test.go` (new)
- Deliver:
  - Register all metrics from spec §6 on a `*prometheus.Registry`
    constructed by `New()` (avoid global default registry — enables tests).
  - `ProjectLabelMode` enum (`all` / `breaker` / `off`) driven by
    `ROP_METRICS_PROJECT_LABEL`. `New(cfg Config)` reads the mode from
    `Config` and registers a different set of collectors accordingly:
    - `all`: every project-scoped metric registered *with* `project` label.
    - `breaker`: only `renovate_project_backoff_seconds`,
      `renovate_project_consecutive_failures`, and
      `renovate_container_exits_total{outcome=failure|rapid_failure}` carry
      the `project` label. Others are registered without it.
    - `off`: no metric carries `project`. All success/dispatch counters are
      aggregate-only.
  - Exported helpers accept `project string` unconditionally; the mode is
    applied *inside* the helper (label dropped or kept) so callers stay
    dumb. Helpers:
    `RecordDispatch(project, result)`, `SetBreakerState(state)`,
    `SetProjectBackoff(project, seconds)`,
    `SetConsecutiveFailures(project, n)`, `ObserveContainerDuration(...)`,
    `RecordDiscovery(result, dur)`, `SetQueueDepth(n)`.
  - `Handler() http.Handler` returns `promhttp.HandlerFor(reg, opts)`.
  - HTTP middleware records duration histogram + status code counter with
    normalised path (avoid cardinality via `mux.CurrentRoute().GetPathTemplate()`).
- Tests:
  - Metric registration succeeds without panic in each of the three modes.
  - `RecordDispatch` increments correct label combo in `all` mode.
  - In `breaker` mode: success dispatches have no `project` label; failure
    ones do. Assert via `testutil.CollectAndCount` / `CollectAndCompare`.
  - In `off` mode: no metric family exposes `project`.
  - Middleware wraps a handler and records one observation.
- Acceptance: `go test ./internal/metrics/...` green.

### T6 — executor hooks

- Files: `internal/executor/docker.go`, `internal/executor/docker_test.go`.
- Deliver:
  - Extend `Executor` (or the constructor) to accept optional
    `resilience.Reporter` and `metrics.Recorder` interfaces (defined in
    respective packages).
  - After container wait, classify outcome:
    - `Success` if exit == 0.
    - `RapidFail` if exit != 0 AND runtime < `FailureMinRuntime`.
    - `SlowFail` otherwise.
  - Call `Report(project, source, outcome)` and
    `metrics.ObserveContainerDuration(...)`.
  - Discovery containers report with a synthetic `project = "__discovery__"`
    (feeds breaker but is filtered out of per-project UI listing).
- Tests:
  - Fake resilience.Reporter records the classification for each of the
    three scenarios via a stub Docker runtime layer.
  - No hook wired ⇒ no-op (nil-safe).
- Acceptance: docker executor tests still pass; new classification tests
  green.

### T7 — scheduler / dispatcher gate

- Files: `internal/scheduler/scheduler.go`, `_test.go`.
- Deliver:
  - Constructor takes a `resilience.Gate` interface.
  - Before dispatching (cron OR manual/webhook path), call
    `AllowDispatch(project, source)`.
  - `allowed=false` → skip project, log at INFO with reason + retryAfter,
    increment `renovate_dispatch_total{result=reason}`.
  - Manual API path calls `MarkManual(project)` FIRST, then normal
    dispatch — the manager consumes the flag inside `AllowDispatch`.
- Tests:
  - Cron dispatch skipped when backoff active.
  - Manual dispatch bypasses backoff.
  - Manual dispatch bypasses open breaker.
  - Dispatch counter increments correct labels.
- Acceptance: existing scheduler tests still pass.

### T8 — HTTP surface

- Files: `internal/server/server.go`, `internal/api/*` (extend as needed),
  `internal/server/server_test.go`, `internal/webhook/forgejo.go`.
- Deliver:
  - `GET /metrics` → `metrics.Handler()`.
  - `POST /api/v1/breaker/reset` → calls `Manager.Reset(actor="api")`;
    fans out `JobStatusScheduled` writes for every project in
    `ResetSummary.ReplayedProjects` via the existing state store;
    responds with `{message, previousState, replayedProjects,
    clearedBackoffs}`.
  - `GET /api/v1/breaker` → returns the Snapshot from T4 as JSON (includes
    `pendingReplayCount`).
  - **Webhook change:** `internal/webhook/forgejo.go` no longer returns 503
    when `AllowDispatch` denies. Instead it calls
    `Manager.EnqueueWebhookReplay(project)` and returns **202 Accepted**
    with `{status:"queued", reason, project}`. The rare cap-exceeded case
    (queue full) still returns 503.
  - Wire HTTP middleware from T5 around the mux.
- Tests:
  - Reset endpoint clears breaker and returns replayed projects list
    (integration test with real Manager + injected clock + fake state store).
  - `/metrics` returns 200 with `text/plain; version=0.0.4`.
  - Breaker endpoint returns snapshot fields including `pendingReplayCount`.
  - Webhook returns 202 when breaker open and project is queued.
  - Webhook returns 503 only when replay queue is at cap.
- Acceptance: server test suite green.

### T9 — static UI updates

- Files: `static/index.html` (+ any inline React pages), possibly
  `static/js/*` and `static/components/*`.
- Deliver:
  - Poll `/api/v1/status` (existing) — read new breaker fields.
  - Banner at top of page when `state=open`:
    "Circuit breaker open — cron dispatches paused. [Reset]".
  - Per-project row shows a pill: `Backing off Xs` when backoff active,
    `Manual queued` when flag pending.
  - Reset button posts to `/api/v1/breaker/reset`, on success reloads
    status.
- Tests: none automated (React inline w/ Babel), but a manual smoke script
  in the plan verifies rendering. Screenshots deferred to review.
- Acceptance: page loads without console errors; banner renders when server
  returns `state:"open"`.

### T10 — docs + integration smoke

- Files:
  - `README.md` — add "Resilience" and "Metrics" sections referencing env
    vars from spec §7.
  - `unipi/docs/specs/2026-07-29-resilience-metrics.md` — link plan.
  - `internal/integration_test.go` (new, build-tag `integration`):
    - Spin up server with a stub executor that fails N containers rapidly,
      assert breaker opens, `/metrics` reflects it, reset closes it.
- Acceptance: `go test -tags=integration ./...` green.

## Config env vars (new)

Documented in spec §7; implemented in `config/config.go`:

```
ROP_FAILURE_MIN_RUNTIME=30s
ROP_BACKOFF_BASE=30s
ROP_BACKOFF_MAX=30m
ROP_RAPID_FAIL_WINDOW=5m
ROP_RAPID_FAIL_THRESHOLD=10
ROP_METRICS_PROJECT_LABEL=all   # all | breaker | off
```

Each parsed via existing helpers in `config/config.go`; defaults applied by
`resilience.Config` when zero.

## Test strategy

- Pure logic (Tasks 1–5) → unit tests, table-driven, injected clock.
- Wiring (Tasks 6–8) → interface stubs, no real Docker.
- End-to-end (Task 10) → build-tagged integration test with fake executor.
- `-race` on the resilience package specifically because it's the only
  shared-state concurrent bit.

## Rollout / risk

- No persisted state → deploy = restart = clean slate. Safe to roll forward
  or back.
- Existing dispatch path is preserved for projects where the manager returns
  `allowed=true`. If we discover regressions in the field, set
  `ROP_RAPID_FAIL_THRESHOLD=999999` to effectively disable the breaker.

## Out of scope for this plan

- Persisting breaker/backoff state.
- Per-project circuit breaker (only global).
- Alertmanager rules, Grafana dashboards.
- Retry-with-backoff on platform HTTP calls.
- Auth on `/metrics`.

## Acceptance for the whole feature

1. All 10 tasks landed on `feat/resilience-metrics`.
2. `go build ./... && go test ./... -race` green.
3. `go test -tags=integration ./...` green.
4. Manual smoke: trigger 10 rapid failures via stub, confirm breaker banner
   appears in UI, reset closes it, `/metrics` shows the expected series.
5. Spec §11 "Acceptance criteria" list all checked.

## Implemented

| Task | Commit(s) |
| --- | --- |
| T1 | 06d3c9a |
| T2 | 9c63375 |
| T3 | d4986ca |
| T4 | 6c1bb46 |
| T5 | 6662fab |
| T6 | 020809b |
| T7 | 3b69e96 |
| T8 | f4cc49a, 86291e8, 6fc5718 |
| T9 | (pending) |
| T10 | (this commit) |
