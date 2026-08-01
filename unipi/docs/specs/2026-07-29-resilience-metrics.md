# Spec: Resilience (backoff + circuit breaker) & Prometheus metrics

Date: 2026-07-29
Status: Implemented (2026-07-30). See plan for commit references.
Owner: /unipi:auto

## 1. Problem

The operator dispatches Renovate containers on a cron. Two production gaps:

1. **Failure floods**. If Renovate is misconfigured (bad token, unreachable
   platform endpoint, missing image, corrupted cache, etc.) every scheduled
   container fails almost immediately. Today the executor happily re-dispatches
   on the next cron tick, hammering the Docker daemon and platform. Nothing
   backs off, nothing stops.

2. **No metrics endpoint**. Operators cannot scrape run rates, failure rates,
   dispatch latency, breaker state, or Go runtime health with Prometheus. The
   only signal is the SQLite state + logs.

We want (a) a per-project backoff, (b) a global circuit breaker that stops
scheduled work when the failure pattern is systemic, (c) a manual + restart
reset path, (d) a `/metrics` endpoint with business + Go runtime metrics.

## 2. Goals

- Detect "rapid failure": non-zero exit with runtime < threshold (default 30s).
- Per-project exponential backoff `min(30s * 2^(fails-1), 30m)`, reset on success.
- Global circuit breaker: if ≥3 rapid failures across all projects within a
  5-minute rolling window, open the breaker. Once open, stays open until reset.
- When breaker is open:
  - Cron-based dispatch is blocked (no new `Scheduled` transitions from cron).
  - Webhook triggers return HTTP 503.
  - Manual UI/API triggers (`POST /api/v1/renovate`, `POST /api/v1/renovate/all`)
    are allowed — they act as override.
- Reset paths:
  - UI button in a new "Resilience" panel (shown only when breaker open, with
    breaker state banner regardless).
  - `POST /api/v1/breaker/reset` REST endpoint.
  - Container restart implicitly resets (state is in-memory).
- New `GET /metrics` handler on the existing HTTP port (same mux, no auth).
- Metric scope: container lifecycle, discovery, queue, HTTP duration, breaker
  state, plus default `go_*` / `process_*` collectors.

## 3. Non-goals

- Persisting breaker/backoff state across restarts.
- Per-project circuit breaker (only per-project backoff; breaker is global).
- `/metrics` auth: none. The operator is deployed on trusted internal
  networks and the rest of the HTTP surface (`/api/*`) is also unauth. Users
  who need isolation can restrict via NetworkPolicy / firewall.
- Alertmanager rules / Grafana dashboards (docs pointer only).
- Retry-with-backoff for failing HTTP calls to the platform API. This is only
  about container-level failures.

## 4. Definitions

- **Rapid failure**: any container (executor OR discovery) exits with a
  non-zero exit code AND `exitedAt - startedAt < ROP_FAILURE_MIN_RUNTIME`
  (default 30s). Feeds both the per-project (or per-job for discovery)
  backoff AND the global rapid-fail window.
- **Slow failure**: exit != 0 with runtime ≥ threshold. Feeds the per-project
  backoff (same exponential curve) but does NOT feed the global breaker
  window. Rationale: slow failures indicate config/code problems, not
  infrastructure crash-loops, so we want to dampen retry pressure on the
  bad project without freezing the whole operator.
- **Success**: exit == 0. Resets the per-project failure counter and clears
  `nextAllowed`.
- **Rapid-fail window**: rolling 5 minute window (configurable) used for the
  global breaker. Only rapid failures land here.
- **Backoff**: earliest wall-clock time at which a project may be re-dispatched.

## 5. Design

### 5.1 New package: `internal/resilience`

```
internal/resilience/
    resilience.go       // Manager (public API)
    resilience_test.go
```

Public API:

```go
type State string
const (
    StateClosed  State = "closed"   // normal
    StateOpen    State = "open"     // breaker tripped
)

type Manager struct { /* unexported */ }

// New builds a Manager with the given config. Uses time.Now unless a clock is
// injected (see WithClock for tests).
func New(cfg Config, logger *slog.Logger) *Manager

type Config struct {
    FailureMinRuntime   time.Duration // default 30s
    BackoffBase         time.Duration // default 30s
    BackoffMax          time.Duration // default 30m
    BreakerThreshold    int           // default 3
    BreakerWindow       time.Duration // default 5m
}

// Consulted by dispatch, cron gate, webhook gate.
func (m *Manager) BreakerState() State
func (m *Manager) BreakerOpenSince() time.Time // zero if closed
func (m *Manager) BreakerReason() string       // empty if closed

// Reset returns the pre-reset state so the caller can log/emit.
// Reset is a **full** resilience reset: it closes the breaker, empties the
// rapid-fail window, clears every per-project `consecutiveFailures` and
// `nextAllowedAt`, and drops all `manualOverride` flags. It also drains the
// webhook-replay queue (see §5.7) — those projects are marked Scheduled by
// the caller of `Reset()` (typically the API handler).
func (m *Manager) Reset(actor string) (previous ResetSummary)

// AllowDispatch returns true iff the project may be dispatched right now.
// Reasons: false when breaker open (for cron/webhook path) OR when the
// project's own backoff is still active.
// The `source` parameter chooses gate policy:
//   - "cron": blocked when breaker open, blocked by per-project backoff
//   - "webhook": blocked when breaker open, blocked by per-project backoff
//   - "manual": ignores breaker, still ignores backoff (user override)
func (m *Manager) AllowDispatch(project string, source Source) (allowed bool, retryAfter time.Duration, reason string)

// RecordExit is called from executor.handleContainerExit after every
// executor container. Feeds both per-project backoff (rapid AND slow
// failures) and the global rapid-fail window (only rapid failures).
func (m *Manager) RecordExit(project string, exitCode int, duration time.Duration)

// RecordDiscoveryExit is called after every discovery container. Only
// rapid failures are recorded; they feed the global rapid-fail window.
func (m *Manager) RecordDiscoveryExit(jobName string, exitCode int, duration time.Duration)

// Snapshot for UI + /metrics render.
type Snapshot struct {
    State           State
    OpenSince       time.Time
    Reason          string
    RapidFailures5m int
    Projects        map[string]ProjectState
}
type ProjectState struct {
    Failures     int
    NextAllowed  time.Time // zero if no backoff
    LastExitCode int
    LastAt       time.Time
}
func (m *Manager) Snapshot() Snapshot

type Source string
const (
    SourceCron    Source = "cron"
    SourceWebhook Source = "webhook"
    SourceManual  Source = "manual"
)
```

Internal state:

- `rapidFailures []time.Time` — timestamps of rapid failures inside the
  breaker window. Truncated on each `RecordExit` and on each `BreakerState`
  read. `sync.Mutex` protects everything.
- `projects map[string]*projState` — `{failures, nextAllowed, lastExitCode, lastAt}`.
- `state State`, `openSince time.Time`, `openReason string`.

Transitions:

- Rapid failure → append timestamp; if `len(window) >= threshold` and closed:
  transition to open, set reason `"3 rapid failures within 5m"`.
- Slow failure or success → NOT counted for breaker. Success also resets the
  per-project failure counter and clears `nextAllowed`.
- Reset → set state closed, clear openSince/reason, keep per-project
  counters (they'll drain naturally on next success). Optionally: also clear
  per-project counters. Decision: **also clear per-project counters** on reset
  — otherwise a reset feels partial. Emit log `breaker reset by <actor>`.

### 5.2 Integration points

- `cmd/main.go`
  - Build a `resilience.Manager` before executor construction.
  - Pass it to the executor, the server, and the cron closure.
  - Cron closure wraps `Dispatch()` with `resilience.SourceCron` gate.

- `internal/executor/docker.go`
  - Add `resilience *resilience.Manager` (nil-safe) to `DockerExecutor`.
  - `doDispatch` before dispatching each project consults
    `resilience.AllowDispatch(proj, SourceCron)`. Source is hard-coded to
    `SourceCron` here because `doDispatch` fires from the executor's own tick
    and from `triggerDispatch()`. The manual path skips the check because the
    manual API only calls `UpdateProjectStatus` and lets the same dispatch
    loop pick it up — see Q&A below.

  **Manual bypass question**: since manual triggers write `JobStatusScheduled`
  and the same `doDispatch` picks them up, we need a way to mark a project as
  "manually scheduled, bypass breaker". Options:

  (A) Add a `Source` column to `RenovateStatusUpdate` and thread it through.
  (B) Check request source in the API handler and, if manual, mark the project
      with a per-project override that `AllowDispatch` respects for one run.
  (C) Bypass the dispatch loop entirely for manual triggers — call executor
      directly.

  Chosen: **(B) one-shot manual override.** The API handler, before setting
  status to `Scheduled`, calls `resilience.MarkManual(project)`. `AllowDispatch`
  consults a `manualOverride map[string]bool` — if set, allow and consume
  (clear the flag). Simplest, no schema change, no dispatch-loop bypass.

  - `handleContainerExit` calls `resilience.RecordExit(project, exitCode, duration, ContainerTypeExecutor)`
    for every executor container after computing duration.
  - Discovery paths (`DispatchDiscovery` and `runDiscoveryContainer`) call
    `resilience.RecordDiscoveryExit(jobName, exitCode, duration)` on
    completion. Rapid discovery failures feed the breaker window
    (discovery normally takes 30s-2m, so a <30s failure is truly a startup
    crash: bad token, unreachable endpoint, image pull error).
    Slow discovery failures are ignored (no per-job discovery backoff — a
    manual discovery is user-initiated and the automatic path is cron-gated).

- `internal/webhook/forgejo.go`
  - Before writing scheduled status, call
    `AllowDispatch(project, SourceWebhook)`. If **not** allowed:
    - Call `resilience.EnqueueWebhookReplay(project)` (see §5.7).
    - Return **202 Accepted** with
      `{"status":"queued","reason":"..."}`.
    - Do NOT return 503 — platforms retry aggressively (Forgejo up to ~1h
      exponential; GitHub up to 8 attempts over ~8h). We'd rather absorb
      the event and replay it on breaker reset than eat a retry storm.

- `internal/server/server.go`
  - Add `POST /api/v1/breaker/reset` → calls `resilience.Reset(actor="api")`.
  - Add `GET /api/v1/breaker` → returns `Snapshot()` as JSON for the UI.
  - Extend `RenovateJobInfo` or add a new `/api/v1/breaker` endpoint. **Decision:
    separate `/api/v1/breaker` endpoint** — the state is global, not per job,
    so it doesn't belong on RenovateJobInfo.
  - In `runRenovateForProject` and `runRenovateForAllProjects`, call
    `resilience.MarkManual(project)` before status update. `runRenovateForAllProjects`
    marks every project it batches to.

### 5.3 Config additions

```
ROP_FAILURE_MIN_RUNTIME    duration    30s
ROP_BACKOFF_BASE           duration    30s
ROP_BACKOFF_MAX            duration    30m
ROP_BREAKER_THRESHOLD      int         3
ROP_BREAKER_WINDOW         duration    5m
ROP_RESILIENCE_ENABLED     bool        true
ROP_METRICS_PROJECT_LABEL  enum        all
```

`ROP_RESILIENCE_ENABLED=false` disables all gating (AllowDispatch always
allows). Metrics still record exits — the flag only affects gate behavior.
Useful for rollback.

### 5.4 Prometheus metrics

New package `internal/metrics`:

```
internal/metrics/metrics.go       // registration
internal/metrics/http.go          // http.Handler middleware for duration
internal/metrics/metrics_test.go
```

Uses `github.com/prometheus/client_golang v1.x`. The `promhttp.Handler()`
already includes `go_*` and `process_*` collectors via the default registry.

Business metrics (all prefixed `renovate_`):

**Project label scope** — controlled by `ROP_METRICS_PROJECT_LABEL`:

- `all` (default): every project-scoped metric carries `project="..."`.
  Best for small/medium deployments (< ~1k projects). Full drill-down in
  Prometheus/Grafana.
- `breaker`: only breaker-relevant metrics carry the label
  (`renovate_project_backoff_seconds`, `renovate_project_consecutive_failures`,
  `renovate_container_exits_total{outcome=~"failure|rapid_failure"}`).
  Success paths drop the label. Cuts cardinality proportional to the
  ratio of failing-to-total projects.
- `off`: no `project` label anywhere. Global aggregates only. Per-project
  detail available only via the UI / `/api/v1/status`.

The knob is applied at metric registration in `internal/metrics`. Metric
*names* are stable across modes; only the label set differs. When a project
label is dropped, the corresponding label value is folded into a synthetic
`"__aggregate__"` bucket (Prometheus does not allow variable-arity labels
on the same metric family, so we register a different set of collectors per
mode at startup).

| Name | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `renovate_container_starts_total` | counter | `type` (executor/discovery), `job` | started containers |
| `renovate_container_exits_total` | counter | `type`, `job`, `outcome` (success/failure/rapid_failure) | exited containers |
| `renovate_container_duration_seconds` | histogram | `type`, `job`, `outcome` | container runtime distribution |
| `renovate_projects_scheduled` | gauge | (no labels) | current count of `Scheduled` projects |
| `renovate_projects_running` | gauge | (no labels) | current count of `Running` projects |
| `renovate_discovery_repos` | gauge | `job` | last discovery repo count |
| `renovate_discovery_last_duration_seconds` | gauge | `job` | last discovery run duration |
| `renovate_rapid_failures_total` | counter | (no labels) | total rapid failures observed |
| `renovate_circuit_breaker_open` | gauge | (no labels) | 1 if open, 0 if closed |
| `renovate_circuit_breaker_transitions_total` | counter | `to` (open/closed) | transitions |
| `renovate_project_backoff_seconds` | gauge | `project` | remaining backoff (0 if none) |
| `http_request_duration_seconds` | histogram | `route`, `method`, `code` | HTTP handler duration |

Registration in `cmd/main.go`. Update sites:

- Executor `pullImage`+`ContainerStart` success → `renovate_container_starts_total{type,job}.Inc()`.
- `handleContainerExit` → increment `renovate_container_exits_total`, observe
  duration histogram, increment `renovate_rapid_failures_total` if rapid.
- `resilience.Manager` toggles `renovate_circuit_breaker_open` gauge on
  transition and increments transitions counter.
- HTTP middleware wraps every mux route. Route name comes from
  `mux.CurrentRoute(r).GetPathTemplate()`; fallback `"unknown"`.
- Gauges for scheduled/running are updated on each status transition in the
  state store, OR polled on a 5-second ticker. **Decision: 5-second ticker**,
  simpler and avoids touching the state-store write path.

`renovate_project_backoff_seconds` is computed on scrape via a `Collector`
implementation (not a static gauge), so it always reflects current wall time.

### 5.5 UI additions

- New collapsible "Resilience" section on the main dashboard, above the job
  table. Shows:
  - Colored badge: `closed` (green) / `open` (red) with `openSince` and `reason`.
  - `Rapid failures (5m): N`.
  - When open: `[Reset breaker]` button; confirms via native `confirm()`, then
    `POST /api/v1/breaker/reset`.
  - Per-project backoff column in the job table: extra small badge `backoff
    (Xs)` next to the project name when `nextAllowed` in the future.
- Refresh: reuse the existing dashboard poll loop; add
  `fetch('/api/v1/breaker')` and merge into a `breaker` state prop.

`static/index.html` is a single Babel-transpiled React app. All UI changes go
into the existing components; no new files.

### 5.6 Concurrency & ordering

- `Manager` uses a single `sync.Mutex`. All methods are cheap (map ops + slice
  truncation). No cross-package locks needed.
- `RecordExit` is called from `handleContainerExit`, which itself is bounded
  by `exitPool` in `listenEvents` (executor.go). So concurrency is capped.
- `AllowDispatch` is called from `doDispatch` (single goroutine per tick) and
  from webhook/API handlers. Cheap.
- **Manual override semantics**:
  - `MarkManual(project)` sets `manualOverride[project] = true`.
  - The FIRST `AllowDispatch(project, _)` call after `MarkManual`, regardless
    of source, consumes the flag and returns allowed=true. Bypasses BOTH
    the breaker AND the per-project backoff.
  - Consumed on first check even if parallelism blocks the actual dispatch.
    Simple mental model; user can re-click if needed.
  - No expiry needed — single-shot removes stale-flag risk.

### 5.7 Reset semantics

`Reset` is a **full resilience reset** — single button, single mental model:

- Breaker: `state → closed`, `openSince/reason → zero`, rapid-fail window
  emptied.
- Per-project state: for every project, `consecutiveFailures → 0` and
  `nextAllowedAt → zero`.
- Manual overrides: all flags cleared (they're one-shot anyway, but a reset
  wipes any that were set-but-not-yet-consumed).
- Webhook replay queue: drained atomically. Every project in the queue is
  written back as `JobStatusScheduled` by the caller of `Reset()` (the API
  handler receives the drained list in `ResetSummary.ReplayedProjects` and
  fans out the state-store updates).
- Emits log: `breaker reset by actor=<actor> replayed=N`.
- Container restart: state is in-memory, so a fresh process starts closed
  with an empty queue — equivalent to `Reset` at boot.

Rationale: the breaker being open is usually *caused* by many projects
backing off. Closing only the breaker would leave those projects idle,
which is confusing UI. A single "Reset" button that clears everything
matches the ops mental model ("the underlying problem is fixed, try
everything again").

### 5.8 Webhook replay queue

- New in-memory set on the Manager: `pendingReplay map[string]struct{}`
  (guarded by the same mutex).
- `EnqueueWebhookReplay(project)` adds to the set. Idempotent.
- Drained by `Reset()` (returned in `ResetSummary.ReplayedProjects`).
- Also drained opportunistically: when `AllowDispatch(project, _)` returns
  `allowed=true` for a project in the set, remove it from the set (the
  project got picked up naturally, no replay needed).
- Bounded: capped at 10,000 entries. Beyond that, `EnqueueWebhookReplay`
  returns an error and the webhook handler returns 503 (rare backstop —
  10k queued webhooks means something is very wrong).
- Not persisted: on container restart the queue is lost, but so is all
  other state, and the next cron/discovery tick will re-schedule any
  active project anyway.

### 5.9 Startup behavior (no grace period)

The operator does **not** implement a startup grace period. Rapid failures
count toward the breaker window from t=0. Rationale:

- If the environment is genuinely broken (bad token, unreachable platform,
  bad image), we want the breaker to trip within ~60s so the operator stops
  spinning containers and ops sees the red banner.
- The alternative — a 60s grace period — delays that visibility and burns
  strictly more container CPU on a broken deployment.
- Rolling restarts (e.g., Kubernetes pod replacement) already give the
  operator a fresh, closed breaker on every start; there's no "warmup
  transient" state to protect against.
- Consequence: a misconfigured operator will boot straight into breaker-open
  within the first minute. This is the intended UX — the UI banner and
  `/metrics` immediately reflect "something is wrong", and the ops loop is
  fix-and-reset.

## 6. API changes

```
POST /api/v1/breaker/reset
  Request: {} (empty body)
  Response 200: {
    "message": "breaker reset",
    "previousState": "open",
    "replayedProjects": ["my/repo", "org/other-repo"],
    "clearedBackoffs": 5
  }

GET /api/v1/breaker
  Response 200: {
    "state": "closed",
    "openSince": null,
    "reason": "",
    "rapidFailures5m": 0,
    "pendingReplayCount": 0,
    "projects": {
      "my/repo": {"failures": 2, "nextAllowed": "2026-07-29T12:00:00Z", "lastExitCode": 1, "lastAt": "2026-07-29T11:58:00Z"}
    }
  }

GET /metrics
  Response 200: Prometheus text exposition (v0.0.4)
```

Webhook `POST /webhook/v1/forgejo` gets a new success mode when the breaker
is open or the project is in backoff:

```
Response 202: {"status":"queued","reason":"circuit breaker open","project":"my/repo"}
```

The project is added to the in-memory replay queue and marked `Scheduled`
automatically the next time `Reset()` runs (or when its per-project backoff
expires — whichever is sooner). No 503 is ever returned to a webhook: we
prefer to absorb and replay over triggering platform-side retry storms.

Manual `POST /api/v1/renovate` and `POST /api/v1/renovate/all` are unchanged
in signature and always return 200 as today (they mark projects Scheduled AND
mark them for manual bypass).

## 7. Testing

- `internal/resilience/resilience_test.go`
  - Unit tests with injectable clock (accept a `func() time.Time` in Config).
  - Cases: no failures → closed; rapid failure count below threshold → closed;
    3 rapid within window → open; 3 rapid but spanning > window → closed;
    per-project backoff exponential curve; success resets counter; manual
    bypass consumed once then cleared; reset closes and clears counters;
    disabled config → always allow.
- `internal/metrics/metrics_test.go`
  - Register collectors, scrape via `promhttp.Handler`, assert expected names
    appear.
- `internal/executor` — extend `docker_test.go` (if it exists) with a fake
  Manager and assert `RecordExit` is called with the right outcome. If the
  file doesn't exist we skip this and rely on manager unit tests.
- `internal/server/server.go` handler tests for `/api/v1/breaker`,
  `/api/v1/breaker/reset`, and `/metrics`.

## 8. Rollout / rollback

- `ROP_RESILIENCE_ENABLED=false` disables gating in one env var.
- Metrics are always on; they carry no side effects.
- Migration: none. In-memory state only.

## 9. Open questions

- None outstanding — all decisions locked via three grilling rounds:
  1. **Failure classification & discovery scope** — rapid < 30s runtime;
     slow fails hit per-project only; discovery containers feed the breaker.
  2. **Manual override & cardinality** — single-shot bypass, no expiry;
     `ROP_METRICS_PROJECT_LABEL` tunes project-label cardinality (default `all`).
  3. **Reset scope, webhook policy, startup behavior** — reset is full
     (breaker + backoffs + manual flags + replay queue); webhooks return
     202 and are replayed on reset; no startup grace period.

## 10. Risks

- Manual bypass is single-shot and consumed on first check; see §5.6. No
  stale-flag risk.
- `renovate_project_backoff_seconds` and `renovate_dispatch_total` are
  labelled by `project` when `ROP_METRICS_PROJECT_LABEL=all` (default).
  Cardinality = projects × labelled metrics. At 500 projects that's ~1.5k
  series across the three per-project metrics, which Prometheus handles
  comfortably. Deployments with thousands of repos should set
  `ROP_METRICS_PROJECT_LABEL=breaker` (per-project detail only on
  breaker-relevant series) or `=off` (aggregates only). This is a
  *documented, ops-tunable* design choice — not an open question.
- `promhttp.Handler` is a global default registry. If a test double registers
  a duplicate metric, tests panic. Use `prometheus.NewRegistry()` in tests.
