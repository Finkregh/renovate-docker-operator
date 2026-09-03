# AGENTS.md — internal/resilience

## What This Package Does

Global **circuit breaker** + per-project **exponential backoff** + **webhook replay queue**.
No retries, no timeouts — those live in the caller (executor/scheduler).

## Key Primitives

| Primitive | Scope | Feeds breaker? |
| ----------- | ------- | ---------------- |
| Per-project exponential backoff (base×2^(n-1), ±20% jitter, capped) | one project | No |
| Global rapid-fail breaker (sliding window counter) | all projects | Yes (rapid only) |
| Manual bypass (`MarkManual` → single-shot flag) | one project | N/A |
| Webhook replay queue (bounded, idempotent set) | global | N/A |

## Caller Contract

1. **Report** after every container run with the correct `Outcome`:
   - `OutcomeRapidFail` = non-zero exit AND runtime < `FailureMinRuntime` (30 s default). Feeds breaker.
   - `OutcomeSlowFail` = non-zero exit AND runtime ≥ threshold. Does NOT feed breaker.
   - `OutcomeSuccess` = exit 0. Resets that project's backoff immediately.
2. **AllowDispatch** before dispatching. Returns `(allowed, retryAfter, reason)`.
   - `reason` is `"breaker_open"` or `"project_backoff"` — callers branch on this string.
3. When `AllowDispatch` denies a webhook dispatch, caller must `EnqueueWebhookReplay` (not 503).
4. **Reset** drains the replay queue → returns `ResetSummary.ReplayedProjects` that the caller must re-schedule.

## Sentinel Error

- `ErrReplayQueueFull` — check with `errors.Is`. Surface 503 to webhook platform only in this case.

## Non-Obvious Behavior

- **Manual bypass consumes on ANY source check**, not just `SourceManual`. First `AllowDispatch` call for that project eats the flag regardless of who asks.
- **`DiscoveryProject` (`"__discovery__"`)** is a synthetic project name. Its rapid failures trip the breaker. Filter it out of user-facing UI listings.
- **All state is in-memory**. Container restart = full reset with no replay.
- **No auto-recovery**. The breaker stays open forever until an explicit `Reset(actor)` call.
- **Opportunistic replay drain**: a successful `AllowDispatch` silently removes the project from the replay queue (no re-schedule needed for that one).

## Tuning Defaults

| Parameter | Default | Effect |
| ----------- | --------- | -------- |
| `BackoffBase` | 30 s | First backoff after initial failure |
| `BackoffMax` | 30 min | Cap on exponential growth |
| `RapidFailWindow` | 5 min | Sliding window for breaker counting |
| `RapidFailThreshold` | 10 | Rapid fails in window to trip breaker |
| `FailureMinRuntime` | 30 s | Boundary between rapid/slow classification |
| `ReplayQueueCap` | 10 000 | Max pending webhook replays |

## Testing Pattern

Inject `Config.Clock` (fake clock) and `Config.Rand` (seeded `*rand.Rand`) for deterministic tests.
See `testManager()` helper in `resilience_test.go`. No wall-clock sleeps needed.
