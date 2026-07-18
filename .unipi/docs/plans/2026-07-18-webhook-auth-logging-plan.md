---
title: "Webhook Auth Logging Enrichment — Implementation Plan"
type: plan
date: 2026-07-18
workbranch: ""
specs:
  - .unipi/docs/specs/2026-07-18-webhook-auth-logging-design.md
---

# Webhook Auth Logging Enrichment — Implementation Plan

## Overview

Enrich the access log middleware with structured authentication fields for webhook requests. When a webhook is processed, the single INFO log line will include `auth_required`, `auth_methods` (which signature headers were tried and their verification result), and `auth_result` (overall ok/failed). Uses the existing `responseCapture` pattern to pass data from handler to middleware.

## Context: Envvar Prefix Rename

The envvar prefix rename (`ROP_*` for operator, `RENOVATE_*` for container passthrough) is being implemented concurrently. This plan uses the **new** envvar names throughout:

- `ROP_WEBHOOK_SECRET` (was `WEBHOOK_SECRET`)
- `ROP_WEBHOOK_ENABLED` (was `WEBHOOK_SERVER_ENABLED`)

No envvars are directly consumed by this feature's code — but tests that set up webhook secrets must use `ROP_WEBHOOK_SECRET`.

## Tasks

- completed: Task 1 — Create `internal/webhook/authcontext.go` with types and helper
  - Description: Create the new file with `AuthAttempt`, `AuthResult` types and the `SetAuthOnResponse` helper function. This establishes the data model that connects the webhook handler to the middleware.
  - Dependencies: None
  - Acceptance Criteria: File compiles; types are exported; `SetAuthOnResponse` accepts `http.ResponseWriter` and `*AuthResult`, safely type-asserts to `*responseCapture`.
  - Steps:
    1. Create `internal/webhook/authcontext.go` with package `webhook`
    2. Define `AuthAttempt` struct with `Type string` and `Verified bool`
    3. Define `AuthResult` struct with `Required bool`, `Methods []AuthAttempt`, `Success bool`
    4. Define `SetAuthOnResponse(w http.ResponseWriter, result *AuthResult)` that type-asserts `w` to a type with an `authResult` field. Since `responseCapture` is in package `server`, use an interface approach: define `type authResultSetter interface { SetAuthResult(*AuthResult) }` and assert against that.
    5. Add JSON tags to `AuthAttempt` (`json:"type"` / `json:"verified"`) for structured log output

- completed: Task 2 — Extend `responseCapture` in middleware.go
  - Description: Add `authResult *webhook.AuthResult` field to `responseCapture` and implement the `SetAuthResult` method so the webhook handler can store auth metadata via the interface.
  - Dependencies: Task 1
  - Acceptance Criteria: `responseCapture` has `authResult` field; implements the interface expected by `SetAuthOnResponse`; no import cycles.
  - Steps:
    1. Add import for `"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/webhook"` in `internal/server/middleware.go`
    2. Add `authResult *webhook.AuthResult` field to `responseCapture`
    3. Add method `func (rc *responseCapture) SetAuthResult(result *webhook.AuthResult) { rc.authResult = result }`
    4. Verify no import cycle (server imports webhook types; webhook's `SetAuthOnResponse` uses an interface — no import of server)

- completed: Task 3 — Modify `authenticate()` to return `AuthResult`
  - Description: Change the signature of `authenticate()` from `bool` to `(bool, *AuthResult)`. Collect each attempted auth method (non-empty header) into `AuthResult.Methods`.
  - Dependencies: Task 1
  - Acceptance Criteria: `authenticate()` returns `(bool, *AuthResult)` with all attempted methods recorded; early-return on first success preserves existing behavior; `AuthResult.Required` is always `true` (caller sets it to `false` for unauthenticated paths).
  - Steps:
    1. Change function signature: `func (h *Handler) authenticate(ctx context.Context, jobID statestore.RenovateJobIdentifier, r *http.Request, body []byte) (bool, *AuthResult)`
    2. Initialize `result := &AuthResult{Required: true, Success: false}`
    3. For each header check (X-Forgejo-Signature, X-Gitea-Signature, X-Hub-Signature-256, Authorization, X-Gitlab-Token):
       - If header is non-empty: attempt verification, append `AuthAttempt{Type: headerName, Verified: ok}` to `result.Methods`
       - If verification succeeds: set `result.Success = true`, return `true, result`
    4. At end (all failed): return `false, result`

- completed: Task 4 — Modify `findAndAuthenticateJob()` to propagate auth result
  - Description: After `authenticate()` returns, store the `AuthResult` on the response writer. Handle the no-auth-required case (set `AuthResult{Required: false}`). Pass `http.ResponseWriter` into `findAndAuthenticateJob`.
  - Dependencies: Task 2, Task 3
  - Acceptance Criteria: `findAndAuthenticateJob` accepts `w http.ResponseWriter` parameter; calls `SetAuthOnResponse` with the result; for unauthenticated jobs, stores `&AuthResult{Required: false}`.
  - Steps:
    1. Change `findAndAuthenticateJob` signature to include `w http.ResponseWriter`: `func (h *Handler) findAndAuthenticateJob(ctx context.Context, jobName, project string, w http.ResponseWriter, r *http.Request, body []byte) (statestore.RenovateJobIdentifier, error)`
    2. In the "no auth required" path (webhook auth disabled): call `SetAuthOnResponse(w, &AuthResult{Required: false})`
    3. In the auth path: capture `ok, authResult := h.authenticate(...)`, then call `SetAuthOnResponse(w, authResult)`
    4. If authentication fails (loop exhausts candidates): ensure the last `authResult` is stored before returning `ErrAuthenticationFailed`

- completed: Task 5 — Update `HandleForgejo()` and `HandleSchedule()` callers
  - Description: Pass `w` to `findAndAuthenticateJob` in both handler functions.
  - Dependencies: Task 4
  - Acceptance Criteria: Both handlers compile and pass `w` to `findAndAuthenticateJob`; no other behavioral changes.
  - Steps:
    1. In `HandleForgejo`: change `h.findAndAuthenticateJob(r.Context(), jobName, project, r, body)` → `h.findAndAuthenticateJob(r.Context(), jobName, project, w, r, body)`
    2. In `HandleSchedule`: same change — add `w` parameter

- completed: Task 6 — Extend `accessLogMiddleware` to log auth fields
  - Description: After `next.ServeHTTP(rc, r)` returns, check `rc.authResult` and append structured auth fields to the log attrs.
  - Dependencies: Task 2
  - Acceptance Criteria: Webhook requests show `auth_required`, `auth_methods`, and `auth_result` in log output; non-webhook requests are unchanged; `auth_methods` is a JSON-serializable slice.
  - Steps:
    1. After `next.ServeHTTP(rc, r)` and before logging, check `if rc.authResult != nil`
    2. Append `"auth_required", rc.authResult.Required`
    3. If `rc.authResult.Required`:
       - Append `"auth_methods", rc.authResult.Methods` (slog will serialize the slice as JSON)
       - Append `"auth_result"` as `"ok"` if `rc.authResult.Success`, else `"failed"`
    4. Add a helper `func formatAuthResult(success bool) string` returning `"ok"` or `"failed"`
    5. Ensure `AuthAttempt` implements `slog.LogValuer` or relies on JSON tags for clean structured output (test with slog JSONHandler)

- completed: Task 7 — Update tests for new `authenticate()` signature
  - Description: Update all existing tests in `internal/webhook/forgejo_test.go` for the new `authenticate()` return signature. Add assertions on the returned `AuthResult`.
  - Dependencies: Task 3
  - Acceptance Criteria: All existing `TestAuthenticate` subtests pass with updated signature; new assertions verify `AuthResult.Methods` content and `AuthResult.Success` field.
  - Steps:
    1. Change all `ok := handler.authenticate(...)` → `ok, authResult := handler.authenticate(...)`
    2. Add basic assertions: `if authResult == nil { t.Fatal("expected non-nil AuthResult") }`
    3. For "X-Forgejo-Signature valid raw hex": assert `authResult.Success == true`, `len(authResult.Methods) == 1`, `authResult.Methods[0].Type == "X-Forgejo-Signature"`, `authResult.Methods[0].Verified == true`
    4. For "no auth headers": assert `authResult.Success == false`, `len(authResult.Methods) == 0`
    5. For "multiple headers — first invalid but second valid": assert `len(authResult.Methods) == 2`, first method `Verified == false`, second `Verified == true`

- completed: Task 8 — Add integration test for middleware auth enrichment
  - Description: Create a test that exercises the full middleware → handler → auth → log chain to verify auth fields appear in structured log output.
  - Dependencies: Task 6, Task 7
  - Acceptance Criteria: Test creates a handler with middleware, sends a webhook request, captures slog output, verifies `auth_required`, `auth_methods`, and `auth_result` fields are present and correct.
  - Steps:
    1. Create `internal/server/middleware_test.go` (new file)
    2. Set up a `slog.JSONHandler` writing to a `bytes.Buffer`
    3. Create `accessLogMiddleware(logger)` wrapping a test handler that calls `SetAuthOnResponse` with a sample `AuthResult`
    4. Send an HTTP request via `httptest`
    5. Parse the JSON log output and assert fields:
       - `auth_required: true`
       - `auth_methods` contains expected entries
       - `auth_result: "ok"` or `"failed"`
    6. Also test a request where no auth result is set (non-webhook path) — verify auth fields are absent

## Sequencing

```
Task 1 (authcontext.go — types)
  ├── Task 2 (middleware.go — responseCapture extension)
  │     └── Task 6 (middleware log enrichment)
  │           └── Task 8 (middleware integration test)
  └── Task 3 (authenticate() returns AuthResult)
        ├── Task 4 (findAndAuthenticateJob propagation)
        │     └── Task 5 (handler callers)
        └── Task 7 (authenticate test updates)
```

**Suggested execution order:** 1 → 2 → 3 → 4 → 5 → 6 → 7 → 8

## Files to Modify

- `internal/webhook/authcontext.go` — **NEW**: types (`AuthAttempt`, `AuthResult`) and `SetAuthOnResponse` helper
- `internal/server/middleware.go` — extend `responseCapture` with `authResult` field and `SetAuthResult()` method; extend `accessLogMiddleware` to log auth fields
- `internal/webhook/forgejo.go` — change `authenticate()` return type; change `findAndAuthenticateJob()` signature to accept `w`; update both handler call sites
- `internal/webhook/forgejo_test.go` — update `TestAuthenticate` for new return signature; add `AuthResult` assertions
- `internal/server/middleware_test.go` — **NEW**: integration test for auth field enrichment in access logs

## New Files

- `internal/webhook/authcontext.go` — Auth result types and response-writer helper
- `internal/server/middleware_test.go` — Middleware integration tests

## Dependencies

| Task | Depends On |
| ------ | ----------- |
| Task 2 | Task 1 |
| Task 3 | Task 1 |
| Task 4 | Task 2, Task 3 |
| Task 5 | Task 4 |
| Task 6 | Task 2 |
| Task 7 | Task 3 |
| Task 8 | Task 6, Task 7 |

## Risks

1. **Import cycle**: `server` → `webhook` (for `AuthResult` type) and `webhook` → `server` (for `responseCapture`) would create a cycle. The design avoids this by using an **interface** in the webhook package (`authResultSetter`) rather than importing the server package. Verify at build time.

2. **slog structured output**: `[]AuthAttempt` may not serialize cleanly with all slog handlers. Need to verify with `slog.JSONHandler` that the output matches the spec's expected format. May need to implement `slog.LogValuer` on `AuthResult` or use `slog.Any("auth_methods", authResult.Methods)`.

3. **Envvar rename timing**: This plan assumes the envvar rename is complete before implementation. If not, test setups using `ROP_WEBHOOK_SECRET` will fail. Mitigation: implement after envvar rename Tasks 1–5 are done, or use current names and let the rename plan fix them.

4. **Handler signature change**: `findAndAuthenticateJob` adding `w http.ResponseWriter` is a breaking change to the internal API. Since it's not exported, this is low risk — but all call sites must be updated atomically (Task 4 + Task 5 together).

5. **Multiple candidates in findAndAuthenticateJob**: The current loop tries auth on multiple candidate jobs. The last `AuthResult` stored may not reflect all attempts across jobs. The design stores the result from the last attempted job, which is acceptable — the auth fields show "what happened on the final attempt" which is most diagnostic.
