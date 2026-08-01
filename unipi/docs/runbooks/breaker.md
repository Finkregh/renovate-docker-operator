# Runbook: Circuit Breaker

## 1. Symptoms

- **UI:** Red banner at the top of the dashboard — "Breaker OPEN".
- **Metrics:** `renovate_breaker_state == 1` (query via Prometheus or `curl $BASE/metrics | grep breaker_state`).
- **Webhooks:** Platform (Forgejo/GitHub) reports 202 responses en masse instead of immediate scheduling.

## 2. Diagnosis Checklist

### Read breaker state

```bash
curl -sf $BASE/api/v1/breaker/state | jq
```

Key fields:

- `state` — `"open"` or `"closed"`.
- `openReason` — human-readable trigger description.
- `pendingReplayCount` — number of queued webhook events waiting for reset.
- `projects` — map of per-project state; look for high `consecutiveFailures`.

### Check operator logs

Look for recent entries with:

- `outcome=rapid_fail` — individual container rapid failures.
- `breaker tripped` or `state=open` — the trip event itself.
- Timestamps correlate with the `openSince` field from the state endpoint.

### Check downstream health

- **Renovate registry:** Can the operator host reach `ghcr.io` / your private registry?
- **Forgejo token:** Is `RENOVATE_TOKEN` still valid? (`curl -H "Authorization: token $TOKEN" $PLATFORM/api/v1/user`)
- **Docker daemon:** Is the Docker socket responsive? (`docker info`)

## 3. Actions

### Cause understood and fixed

```bash
curl -X POST $BASE/api/v1/breaker/reset
```

Response includes `replayedProjects` — these are automatically re-scheduled.
Verify in the UI that projects transition back to `Scheduled` → `Running`.

### Single project stuck, rest healthy

```bash
curl -X POST $BASE/api/v1/breaker/bypass/org/repo
```

This is a **single-shot** manual override. The project runs once, bypassing
both the breaker and its own backoff timer. If it fails again, the backoff
re-arms normally.

### Environment still broken, ops investigating

Leave the breaker open. Webhooks continue to queue (up to `ROP_REPLAY_QUEUE_CAP`,
default 10 000). They will be replayed automatically when you eventually reset.

## 4. Confirmation

After reset:

1. `curl -sf $BASE/metrics | grep renovate_breaker_state` → should show `0`.
2. UI banner disappears within ~10 s (next poll cycle).
3. Previously-queued projects appear as `Scheduled` in the jobs list.

## 5. Known Limitations

- **Replay queue is in-memory.** A process restart drops all queued webhook events.
  If the operator restarts while the breaker is open, those events are lost.
  Forgejo will not re-deliver them (they already received 202).
- **`MarkManual` (bypass) is single-shot.** It is not a persistent whitelist.
  Each bypass must be issued explicitly before the desired run.
- **No startup grace period.** A broken environment on cold start trips the
  breaker within ~60 s (by design). This is intentional — ops should see the
  red banner immediately rather than wasting resources on doomed containers.
