# Renovate Docker Operator — Development Guide

This document covers project structure, coding conventions, architectural decisions, and everything a contributor needs to work on this codebase.

## Project Overview

A standalone Docker-based operator that runs [Renovate](https://github.com/renovatebot/renovate) for automated dependency updates. It manages cron scheduling, parallel job execution, webhook-driven triggers, and a web UI — all while abstracting over multiple Git platforms.

## Directory Structure

```
cmd/                       # main.go — wires all components together
config/                    # Singleton env-var config with schema validation
internal/
├── api/                   # Standalone type definitions (RenovateJob, specs, status)
├── discovery/             # Project autodiscovery agent (runs Renovate container)
├── executor/              # Docker container lifecycle (create, dispatch, events, cleanup)
├── parser/                # Renovate JSON log parser (PR activity, warnings, errors)
├── scheduler/             # Cron scheduling wrapper (robfig/cron)
├── server/                # HTTP server (UI, API, webhook routing, OIDC auth)
├── statestore/            # SQLite state store (WAL mode) + RenovateJobManager interface
└── webhook/               # Forgejo webhook handler (push, PR, issue events)
static/                    # Frontend assets (CSS, JS, React components)
integration/               # Integration tests (webhook signing, over-the-wire)
```

## Building & Testing

```bash
# Build the binary
just build

# Run all tests
just test-unit

# Run tests with race detection (quick iteration)
go test -count=1 -race ./...

# Run integration tests (webhook signing, over-the-wire)
just test-integration

# Lint + test in one command
just check

# Run locally (requires Docker and ROP_PLATFORM_ENDPOINT)
export ROP_PLATFORM_ENDPOINT="https://git.example.com"
export ROP_TOKEN="your-token"
export ROP_SQLITE_PATH="./test.db"
just run
```

### Test Coverage

| Package | Tests | Style |
| --------- | ------- | ------- |
| `internal/statestore` | SQLite operations, migrations, HMAC validation | Unit + integration |
| `internal/webhook` | Forgejo event filtering, signature validation, scheduling | E2E with httptest |
| `internal/parser` | Renovate log parsing, PR activity extraction | Unit (table-driven) |
| `internal/server` | HTTP routing, API endpoints | E2E with httptest |
| `internal/scheduler` | Cron scheduling, start/stop lifecycle | E2E with real cron |
| `internal/executor` | Env var construction, name sanitization, Docker stream detection | Unit (table-driven) |
| `internal/discovery` | Constructor safety, nil handling | Unit |
| `integration/` | Webhook signing token validation over HTTP | Integration |

### Verification Commands

- `just build` — compile the project
- `just test-unit` — run all unit/E2E tests via gotestsum
- `just test-integration` — run integration tests (requires running instance)
- `just check` — lint + test in one command
- `just golangci-lint` — run linter only

## Coding Conventions

### 1. Program to Interfaces

**Any component that touches external systems should be expressed as an interface.** This enables testing and future extensibility.

- Key interface: `statestore.RenovateJobManager` — all state access goes through this
- The `executor.DockerExecutor` is currently a concrete struct (Docker SDK dependency); future refactoring may extract an interface for testability

### 2. Docker Container–Based Execution

Renovate runs are launched as Docker containers via the Docker SDK. Key patterns:

- Containers are labeled with `renovate-standalone/*` labels for identification and lifecycle tracking
- The executor listens to Docker events (container `die`) to handle exit processing
- Orphan reconciliation runs periodically to re-adopt or clean up missed containers
- Container names are generated via `sanitizeName()` — always use it for Docker-safe naming

### 3. Configuration

All configuration is environment-variable driven via the singleton in `config/`. Rules:

- Declare new config values in the config schema (with `Optional`/`Required` and defaults)
- Access config values via `config.GetValue()` — never read `os.Getenv` directly elsewhere
- The operator reads `ROP_TOKEN` and passes it to containers as `RENOVATE_TOKEN`
- All `RENOVATE_*` env vars from the operator process are passed through to containers (always override)

### 4. Error Handling

- Use `fmt.Errorf("context: %w", err)` for wrapping and propagating errors in normal paths
- Fail open on external API errors where exclusion would be worse than inclusion
- Container failures are tracked per-project in the state store with exit codes

### 5. Concurrency

- Use `sync.Mutex` / `sync.RWMutex` to protect shared state (see `executor.DockerExecutor.mu`)
- The executor polls every 10 seconds and respects the `parallelism` config
- Docker event listener uses a bounded goroutine pool (`exitPool`) to handle concurrent container exits

### 6. Logging

Use `log/slog` throughout (injected via constructor, never obtained globally). Follow these conventions:

- `logger.Info("message", "key", value)` for normal operational events
- `logger.Error("message", "error", err)` for errors with context
- `logger.Warn("message", ...)` for non-fatal issues
- Never use `fmt.Println` or `log.Print` in production code paths

### 7. Health Checks

Health is exposed via `/healthz` endpoint on the HTTP server. The executor verifies Docker connectivity via `Ping()` at startup.

### 8. Naming Conventions

- Docker container names: `renovate-<sanitized-project>-<unix-timestamp>`
- Discovery containers: `renovate-discovery-<sanitized-job>-<unix-timestamp>`
- Labels: `renovate-standalone/project`, `renovate-standalone/job-name`, `renovate-standalone/type`

## Technology Stack

| Concern | Library |
| --------- | ---------- |
| Container orchestration | `github.com/docker/docker` (Docker SDK) |
| Scheduling | `github.com/robfig/cron/v3` |
| HTTP routing | `github.com/gorilla/mux` |
| State persistence | `modernc.org/sqlite` (pure-Go SQLite) |
| Logging | `log/slog` (standard library) |
| OIDC auth | `github.com/coreos/go-oidc` + `golang.org/x/oauth2` |

## Key Architectural Decisions

- **Single-process design** — one binary runs the scheduler, executor, webhook server, and UI. No leader election needed (unlike the upstream K8s operator).
- **Platform credentials** are passed via environment variables (`ROP_TOKEN`) — the operator injects them into containers as `RENOVATE_TOKEN`.
- **Webhook server** handles Forgejo events at `/webhook/v1/forgejo?job=<name>`. Events are validated (HMAC-SHA256 or Standard Webhooks signatures), filtered (branch, event type), and then schedule projects for immediate execution.
- **Webhook sync is stateless** — after each discovery run, `statestore.SyncWebhooks` ensures the operator's webhook exists on every discovered project and removes it from repos that were removed during reconciliation. Hooks are identified by their delivery URL. Sync failures are logged, never block discovery (fail open).
- **Discovery uses Renovate itself** — a discovery container runs Renovate with `autodiscover: true` and writes discovered repos to a JSON file, which is read from the container's stdout.
- **Executor dispatch loop** — polls every 10 seconds, collects all `scheduled` projects sorted by priority (descending) then oldest-wait, and dispatches Docker containers up to the parallelism limit. Container exit events trigger immediate re-dispatch.
- **Global parallelism limit** — `ROP_GLOBAL_PARALLELISM` env var caps total concurrent Renovate containers. Per-job `Spec.Parallelism` is still enforced as an additional gate.
- **Anti-starvation via priority-then-oldest-wait sort** — candidates are sorted first by `Priority` descending, then by the oldest `LastRun` time. Among equal-priority candidates, the job that has been waiting longest dispatches first.
- **UI sub-path (`ROP_BASE_PATH`)** — the UI, API, auth and health routes can be served under a sub-path. `server.go` mounts all routes on a `PathPrefix(basePath)` subrouter; the frontend builds all runtime URLs from `window.__BASE_PATH__`.
- **Docker stream demultiplexing** — container logs may be in Docker multiplexed format (8-byte frame headers) or raw (Podman/TTY). The `isDockerMultiplexed()` heuristic detects the format and `stdcopy.StdCopy` demuxes when needed.

## Maintaining This Document

Every change to the project structure, conventions, or architectural decisions should be reflected here. Keep this file as the single source of truth for contributors.
