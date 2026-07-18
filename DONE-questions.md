# Open Questions — Scan Remediation Plans — RESOLVED

> All questions resolved 2026-07-17. Plan files updated accordingly.

---

## P0 — Unbounded Reads

1. ✅ **Request body limit**: **2 MiB** for both API and webhook endpoints.
2. ✅ **`RENOVATEOP_MAX_REQUEST_BODY`**: Implement now.

## P1 — High Priority

1. ✅ **Race condition fix**: **Option A (mutex hold)** — simpler, sufficient.
2. ✅ **Exit handler pool**: Fixed at `max(parallelism * 2, 8)`. Not configurable. Parallelism is a single global limit (default 2), exit handling is fast.
3. ✅ **containerLogs**: **Consolidate** — delete the package, use executor's `getContainerLogs`.

## P2 — Medium Priority

1. ✅ **CORS/CSRF**: **Real concern** — React SPA served from same origin, fetch() calls with cookie-based sessions. Fix: `Origin` header validation middleware.
2. ✅ **RENOVATE_TOKEN**: **Hard fail** / `os.Exit(1)` with clear error message.
3. ✅ **Double-shell wrapping**: **Confirmed bug.** `discoveryCmd` already starts with `/bin/sh -c '...'`. Fix: remove that prefix from the string, keep `Cmd: []string{"/bin/sh", "-c", discoveryCmd}`.
4. ✅ **Scheduler shutdown timeout**: **60s**.
5. ✅ **Image cache TTL=0**: Disable cache (always call Docker API).
6. ✅ **CancelProjectJob**: Practically safe today (SQLite single-writer + WHERE guard). Fix opportunistically if container-stop logic added.

## P3 — Low Priority

1. ✅ **Start() return type**: Keep as-is. Docker connectivity already validated via `exec.Ping()` before `Start()`. No action needed.
2. ✅ **Type validation**: Follow existing pattern — inline in handlers. No `Validate()` method. Add range checks where needed.
3. ✅ **Secrets in global map**: Keep open. No immediate action.

## General / Cross-cutting

1. ✅ **PR strategy**: Single PR on current branch.
2. ✅ **Test coverage**: Add plan for full test coverage. Prefer end-to-end tests exercising webhook→executor→container flow.
