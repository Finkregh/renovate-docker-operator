# AGENTS.md — internal/scheduler

## Architecture: Two-File Split

- **scheduler.go** — Thin wrapper around `robfig/cron/v3`. Owns the cron lifecycle
  (Add/Start/Stop) and schedule parsing. Contains zero business logic.
- **dispatch.go** — Per-project dispatch loop with a resilience gate. Completely
  independent of cron; it receives a `[]string` of projects and a callback.

The scheduler does NOT directly reference discovery or executor. It is wired in
`cmd/main.go`: cron fires → discovers projects → calls `Dispatcher.DispatchProjects`
→ the provided `DispatchFunc` callback triggers executor/statestore logic.

## Concurrency Model

**There is no worker pool, semaphore, or queue.** `DispatchProjects` runs
**synchronously in a single goroutine** — iterating projects sequentially and
calling `fn(project)` one at a time. Parallelism, if any, lives outside this
package (in the executor or the caller's callback).

This means the cron tick blocks until all projects are dispatched. If `fn` is
slow, the next cron tick queues behind it in `robfig/cron`'s internal scheduler.

## Backpressure & Queue-Overflow

- The resilience `Gate` interface (breaker_open, project_backoff) is the only
  backpressure mechanism. It **skips** projects rather than queueing them.
- There is no internal queue. If the gate denies a project, it's simply not
  dispatched on that tick — no retry buffer exists in this package.
- Metric `SetQueueDepth` is declared on the interface but not called from
  dispatch.go. It is intended for external callers.

## Graceful Shutdown

`Stop()` calls `s.cron.Stop()` which returns a context. A goroutine waits on
that context (signals all running jobs completed). A **hard-coded 60-second
timeout** force-returns if jobs don't finish. This is the only shutdown grace
period — there is no configurable timeout or drain signal.

## Fatal-on-Panic Contract (No cron.Recover)

`New()` constructs the underlying cron with `cron.WithChain()` — an explicit
empty job-wrapper chain that replaces the default `cron.Recover` panic
recoverer. A panic inside a scheduled job therefore propagates to the Go
runtime and terminates the process, satisfying the cross-cutting
fatal-on-panic invariant for background workers. Do NOT re-add
`cron.Recover` or a custom recovering `cron.Logger` here.

## Error Handling in Dispatch

`DispatchProjects` captures the **first error** from `fn` but continues
dispatching all remaining projects. This is intentional — one failing repo
does not block others. Callers must check the returned error.

## Testing Strategy

- **scheduler_test.go**: Uses real `robfig/cron` with `@every 1s` schedules and
  `time.Sleep`-based polling. No clock injection or fake time — tests are
  wall-clock dependent and inherently slow (~2.5s timeout loops).
- **dispatch_test.go**: Fully deterministic. Uses fake `Gate` and `Metrics`
  implementations (struct-based, defined in the test file). No interfaces from
  external packages are mocked — only the local `Gate` and `DispatchMetrics`.

## Non-Obvious Gotchas

- `nil` Gate means **everything is allowed** (no-op gate). `nil` metrics means
  recording is silently skipped. Both are explicitly supported.
- `GetNextRunOnSchedule` uses a **5-field parser** (no seconds field), unlike
  `cron.New()` which uses the standard descriptor parser (supports `@every`).
  These are different parsers — don't assume one's expressions work in the other.
- The `DispatchFunc` callback is not retried on failure. Resilience/retry lives
  in the gate or upstream caller, never inside dispatch.
