# AGENTS.md — internal/metrics

Prometheus metrics registration and HTTP middleware for the operator. Uses `github.com/prometheus/client_golang`.

## Non-obvious: NOT a global registry
- Uses `prometheus.NewRegistry()`, **not** the default `prometheus.DefaultRegisterer`. Every `metrics.New()` call creates a fresh, isolated registry. This means:
  - Tests can create as many `Recorder`s as they want without duplicate-registration panics.
  - The `/metrics` handler must be wired via `promhttp.HandlerFor(recorder.Registry(), ...)` — a bare `promhttp.Handler()` will not see our metrics.
- Go runtime + process collectors are registered on our custom registry (`collectors.NewGoCollector`, `collectors.NewProcessCollector`) — they are **not** free; if you drop the custom registry you lose them.

## Project-label mode
`Config.ProjectLabelMode` controls per-project label cardinality — this is the single most important knob to prevent metric explosion when a job discovers thousands of repos:
- `all` (default) — every metric gets a `project` label. Fine for small deployments.
- `breaker` — only breaker-relevant metrics (`container_exits_total`, `circuit_breaker_*`, `project_backoff_seconds`, `project_consecutive_failures`) keep the label. Success-path counters/histograms drop it. Use this for large fleets.
- `off` — no per-project labels anywhere. Coarsest.
- Unknown mode → `New()` returns an error. Validate at config parse time, not lazily.

The mode is picked up **once at construction** — you cannot change it at runtime. If you add a new metric, decide upfront whether it goes through `projectLabelsAlways()` or `projectLabelsSuccessPath()`.

## Registered metrics
| Name | Type | Labels (mode=all) | Purpose |
|---|---|---|---|
| `renovate_container_starts_total` | CounterVec | type, job, project | Started containers |
| `renovate_container_exits_total` | CounterVec | type, job, project, outcome | Exits by outcome (success/rapid_fail/slow_fail) |
| `renovate_container_duration_seconds` | HistogramVec | type, project, outcome | Runtime, default buckets |
| `renovate_projects_scheduled` | Gauge | — | Scheduled projects count |
| `renovate_projects_running` | Gauge | — | Running projects count |
| `renovate_discovery_repos` | GaugeVec | job | Last discovery repo count |
| `renovate_discovery_last_duration_seconds` | GaugeVec | job | Last discovery runtime |
| `renovate_rapid_failures_total` | Counter | — | Feeds the breaker window |
| `renovate_circuit_breaker_open` | Gauge | — | 1 open, 0 closed |
| `renovate_circuit_breaker_transitions_total` | CounterVec | from, to | Breaker state changes |
| `renovate_project_backoff_seconds` | GaugeVec | project (breaker/all) | Remaining backoff |
| `renovate_project_consecutive_failures` | GaugeVec | project (breaker/all) | Consecutive failures |
| `renovate_dispatch_total` | CounterVec | result | Dispatch attempts |
| `renovate_queue_depth` | Gauge | — | Webhook replay queue depth |
| `http_request_duration_seconds` | HistogramVec | route, method, code | HTTP middleware |

## HTTP middleware (`http.go`)
- `Recorder.WrapHandler(pattern, h)` wraps a handler and observes `http_request_duration_seconds` with **static `route` label** = the `pattern` you pass in. **Never** pass `r.URL.Path` as the route — that would explode cardinality. If you add new endpoints, pass a stable pattern like `/api/v1/renovate` or `/api/v1/breaker/bypass/{project}`.
- HTTP metrics are registered lazily on first `WrapHandler` call (`initHTTP`) — the registry stays clean for callers that don't use middleware (tests, CLI paths).
- `statusWriter` captures the status code; `Write` without `WriteHeader` still records 200.

## Test gotchas
- Registry is per-Recorder, so tests should build a fresh `Recorder` in each test and assert against its `Registry()`.
- The Recorder is **not** a singleton — `main.go` constructs one and passes it into executor/resilience/server via `Set*` setters (all nil-safe).

## Don't
- Don't add high-cardinality labels (repo URL, commit SHA, PR number, user IDs). If tempted, route them through logs, not metrics.
- Don't call `prometheus.MustRegister` on the default registerer from this package — always use `r.reg`.
