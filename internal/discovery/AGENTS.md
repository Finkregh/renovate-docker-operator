# AGENTS.md — internal/discovery

## What This Package Does
Orchestrates **Renovate autodiscovery** — the process of finding which repositories
need Renovate runs. It does NOT talk to any git platform directly; instead it
delegates to `internal/executor.DockerExecutor` which runs a Renovate container
in discovery mode. The executor returns a `[]string` of repo names.

## Architecture & Data Flow
1. `RunDiscovery` or `RunDiscoveryAsync` is called with a `*api.RenovateJob`.
2. The executor spawns a Docker container that runs Renovate's `--autodiscover`.
3. Results (repo list) are reconciled against the state store (`statestore.ReconcileProjects`).
4. Removed repos are returned so the caller (scheduler) can clean up webhooks.

## Concurrency Model — Pitfalls
- **`RunDiscoveryAsync`** uses the executor's async dispatch with a completion callback.
  The callback runs on the executor's goroutine, NOT the caller's.
- The callback deliberately uses `context.Background()` (not the original ctx) to
  survive cancellation of the triggering request — this is intentional, not a bug.
- There is a **double guard** against concurrent runs: the executor's own locking
  AND a state-store "running" status check. If the executor rejects the run, the
  agent rolls status back to "idle".
- Errors inside the async callback are silently swallowed (logged, status set to
  "failed", but `SetDiscoveryStatus` errors are `_ =` discarded).

## External Dependencies
- `internal/executor` — Docker container dispatch (discovery mode)
- `internal/statestore` — persistence of projects + discovery status
- `internal/api` — shared types (`RenovateJob`)
- **No direct HTTP/API calls to any git forge** from this package.

## Testing Strategy
- Tests are **unit-only** and exercise only the constructor (`New`) and nil-pointer
  panic behavior.
- No mocks of `executor` or `statestore` exist yet — tests rely on nil panics.
- **No httptest, no network calls** — integration testing lives elsewhere.

## Non-Obvious Details
- `executor` and `store` can be nil at construction time without error; the agent
  will panic only when `RunDiscovery` is called. Tests validate this behavior.
- The agent does not handle rate limiting or caching — that responsibility belongs
  to the executor and state store layers.
- `ErrDiscoveryAlreadyRunning` is defined in the `executor` package, not here.
