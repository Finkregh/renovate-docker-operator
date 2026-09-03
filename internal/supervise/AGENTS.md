# AGENTS.md — internal/supervise

Supervised-loop primitives enforcing the cross-cutting invariants for **every** background goroutine in the operator. The whole package exists to make one rule un-forgettable: **a dead ticker is a fatal bug.** Silent goroutine death is the failure mode this package refuses to allow.

## The three invariants

1. **A loop that exits for any reason other than `ctx.Done()` is fatal.** Panic in the body, or a body that returns `nil` while `ctx` is still live → stack trace logged via `slog` + `exitFn(1)`.
2. **Per-tick errors are logged and retried.** A single transient failure (network blip, 5xx) does not kill the loop — it is logged at `warn`, counted, and the loop continues to the next interval.
3. **`ctx.Done()` is the only clean exit.** Cancellation stops the loop quietly with a summary log line.

If you are adding a background goroutine, it goes through `Loop` or `Guard`. Do not hand-roll a bare `for/select` — you will eventually forget the fatal-on-unexpected-exit case, which is exactly the bug this package prevents.

## `Loop` vs `Guard` — which one

- **`Loop(ctx, logger, name, interval, body, opts...)`** — use when you have a `func(context.Context) error` you want run on a ticker. This is the common case (`image-refresher`, `image-update-poller` in `cmd/main.go`). `Loop` owns the ticker and the select.
- **`Guard(ctx, logger, name)`** — use when the caller manages its **own** `for/select` loop and you only want the fatal-on-unexpected-exit guarantee wrapped around it. It is a defer idiom:

  ```go
  defer supervise.Guard(ctx, logger, "dispatchLoop")()
  ```

  Used by `internal/executor` (`dispatchLoop`, `listenEvents`, `reconcileLoop`). It fatals if the surrounding function returns while `ctx` is live, or if a panic is unwinding through the defer.

## `interval == 0` semantics (run-once)

`Loop` with `interval == 0` runs the body **exactly once** and returns — no ticker, and **no fatal on clean nil return**. This is deliberate: it backs the `ROP_IMAGE_UPDATE_POLL_INTERVAL=0` "run once at startup only" mode (see `config.MinImageUpdatePollInterval` and the plan doc §D). Any positive interval gets the immediate-first-tick-then-every-interval ticker with the full fatal contract.

## The fatal-on-early-exit contract (detail)

- **Panic** (in `Loop` body or under a `Guard` defer): recovered, stack trace trimmed to 4 KiB, logged at `error`, then `exitFn(1)`.
- **Nil return with live ctx** (ticker mode only): logged as `"supervise loop body returned unexpectedly"`, then `exitFn(1)`. A body that legitimately wants to keep running must never return nil while `ctx` is live.
- **Error return**: logged at `warn` with the classified `kind`, counted, loop continues. Errors are *never* fatal.

## The `exitFn` seam for tests

`exitFn` defaults to `os.Exit`. Tests must not terminate the test binary, so both entry points expose an override:

- `Loop(..., WithExitFn(fn))` — inject a recording closure.
- `GuardWithExitFn(ctx, logger, name, fn)` — the `Guard` variant.

When `exitFn` is a test no-op (doesn't actually exit), `loopConfig.halted` is set on the fatal path so the loop still breaks instead of spinning — assert on both the `exitFn` invocation count **and** that the loop returned. `cmd/main.go`'s `run(exitFn)` seam threads a test-injected `exitFn` down through here for the same reason.

## The `Metrics` seam — interface only, no concrete impl in this package

`WithMetrics(m Metrics)` injects a recorder implementing:

```go
type Metrics interface {
    IncTick(name string)
    IncError(name, kind string)
}
```

- **This package registers no Prometheus metrics itself.** It defines the *interface* and calls it; the concrete counters live in (or are the responsibility of) `internal/metrics`. Omitting `WithMetrics` — or passing `nil` — yields a `nopMetrics` no-op, so `Loop` never nil-panics on the metric calls.
- The `name` argument is the loop name (`"image-refresher"`, etc.) and is intended as a low-cardinality metric label. Do not pass per-project or per-request strings as `name`.
- **Wiring status:** as of v1 the operator does not call `WithMetrics` — the loops run with `nopMetrics`. The supervise-liveness/error metrics documented in `README.md` describe the intended surface for the `Metrics` implementation; registering them is a `internal/metrics` concern, not a change to this package.

## `ClassifyError` — the low-cardinality label source

`ClassifyError(err) string` maps any error to exactly one of five kinds — **`network`, `context`, `parse`, `auth`, `other`** — suitable as a bounded metric label (`IncError(name, kind)`). Ordering matters:

1. `*net.OpError` / `*url.Error` → `network` (checked first — context errors are often wrapped inside net errors).
2. `context.Canceled` / `context.DeadlineExceeded` → `context`.
3. A `Classify() string` interface on the error (or any wrapped error) → its returned kind, but only if it is one of the four known kinds; anything else collapses to `other`. This is the extension point: define `Classify()` on your error type to control its bucket without touching this package.
4. Message-substring heuristics: auth indicators (`401`, `403`, `unauthorized`, `forbidden`, …) → `auth`; parse indicators (`invalid syntax`, `cannot unmarshal`, `unexpected end of JSON`, …) → `parse`.
5. Fallback → `other`.

Keep the five buckets stable — they are a metric label domain, and adding a sixth kind is a dashboard/alerting change, not just a code change.

## What NOT to do

- Do not swallow a panic or an unexpected exit "to be safe." The fatal is the feature; downgrading it to a log line reintroduces the silent-death bug.
- Do not pass high-cardinality strings as the loop `name` or as an `IncError` `kind` — both are metric labels.
- Do not add a real `os.Exit` path that bypasses the `exitFn` seam — tests rely on every fatal going through `exitFn`.
- Do not register Prometheus metrics here — this package owns the `Metrics` *interface* only; concrete registration belongs in `internal/metrics`.
