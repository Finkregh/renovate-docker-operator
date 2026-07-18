---
title: "Webhook Auth Logging Enrichment"
type: brainstorm
date: 2026-07-18
---

# Webhook Auth Logging Enrichment

## Problem Statement

When webhook requests arrive, the access log shows only HTTP-level info (method, path, status, duration). There is zero visibility into the authentication layer: which signature headers were present, which methods were tried, which succeeded or failed. This makes it impossible to diagnose misconfigured webhooks (e.g., Forgejo Secret field left blank → empty X-Forgejo-Signature → silent failure).

## Context

- The access log middleware (`accessLogMiddleware` in `internal/server/middleware.go`) fires after the handler returns, logging method/path/status/duration/remote.
- Authentication happens inside `authenticate()` in `internal/webhook/forgejo.go`, which returns a bare `bool` with no logging.
- The only auth-related log today is `"webhook rejected: authentication failed"` from `findAndAuthenticateJob`, which gives no detail about what was tried.
- The user's example showed `X-Forgejo-Signature:` (empty) because no Secret was configured in Forgejo — this should be immediately visible in logs.

## Chosen Approach

Extend the existing access log line for webhook paths with structured auth fields. Use request context to pass auth metadata from the handler back to the middleware. One log line per request — no separate auth log lines.

## Why This Approach

- **Single log line per request** — no noise, easy to grep/filter
- **Structured fields** — machine-parseable, filterable in log aggregators
- **Context-based** — clean separation; middleware doesn't need to know about webhook internals
- **Only enriches webhook paths** — non-webhook requests (health, UI, API) remain unchanged

### Alternatives Rejected

1. **Separate log line in handler** — rejected because it doubles the log volume for webhook requests and splits related info across two lines.
2. **Debug-only logging** — rejected because auth state is critical operational info, not debugging detail. The user already has info-level access logs.

## Design

### Data Model

```go
// internal/webhook/authcontext.go

type AuthAttempt struct {
    Type     string // "X-Forgejo-Signature", "X-Gitea-Signature", "X-Hub-Signature-256", "Authorization", "X-Gitlab-Token"
    Verified bool   // whether this method authenticated successfully
}

type AuthResult struct {
    Required bool          // whether auth was required for this job
    Methods  []AuthAttempt // all methods that had non-empty headers (attempted)
    Success  bool          // overall auth outcome
}
```

### Context Key

```go
type ctxKey struct{}

func SetAuthResult(ctx context.Context, result *AuthResult) context.Context {
    return context.WithValue(ctx, ctxKey{}, result)
}

func GetAuthResult(ctx context.Context) *AuthResult {
    v, _ := ctx.Value(ctxKey{}).(*AuthResult)
    return v
}
```

### Changes to `authenticate()`

The function signature changes to return `(bool, *AuthResult)` (or populate context). It collects each attempt:

```go
func (h *Handler) authenticate(ctx context.Context, jobID statestore.RenovateJobIdentifier, r *http.Request, body []byte) (bool, *AuthResult) {
    result := &AuthResult{Required: true}

    if sig := r.Header.Get("X-Forgejo-Signature"); sig != "" {
        ok, _ := h.store.IsWebhookSignatureValid(ctx, jobID, sig, body)
        result.Methods = append(result.Methods, AuthAttempt{Type: "X-Forgejo-Signature", Verified: ok})
        if ok {
            result.Success = true
            return true, result
        }
    }
    // ... same pattern for each method ...

    return false, result
}
```

### Changes to `findAndAuthenticateJob()`

After calling `authenticate()`, store the result in the request context (via a mutable pointer set before the handler call, or by using `http.Request.WithContext`). Since the middleware wraps the handler, the result will be available when the middleware logs after `next.ServeHTTP` returns.

**Implementation detail:** The middleware creates a context-enriched request before calling next. The handler stores auth result in a struct referenced via context pointer (set before handler call via middleware), so no need to modify `r` after handler starts.

Alternative simpler approach: Use a `sync.Map` or request-scoped struct embedded in the response writer wrapper (like `responseCapture` already does for status code).

### Changes to `accessLogMiddleware`

After `next.ServeHTTP(rc, r)` returns, check for auth result in context:

```go
if authResult := GetAuthResult(r.Context()); authResult != nil {
    attrs = append(attrs,
        "auth_required", authResult.Required,
        "auth_methods", formatMethods(authResult.Methods), // [{"type":"X-Forgejo-Signature","verified":false}]
        "auth_result", boolToResult(authResult.Success),   // "ok" or "failed"
    )
}
```

### Log Output Format

```json
{
  "time": "2026-07-18T16:49:27.870Z",
  "level": "INFO",
  "msg": "http request",
  "method": "POST",
  "path": "/webhook/v1/forgejo",
  "status": 202,
  "duration": "261µs",
  "remote": "10.89.11.190:56362",
  "auth_required": true,
  "auth_methods": [
    {"type": "X-Forgejo-Signature", "verified": false},
    {"type": "X-Hub-Signature-256", "verified": true}
  ],
  "auth_result": "ok"
}
```

When auth is not required (webhook_auth_enabled=0):

```json
{
  "...": "...",
  "auth_required": false
}
```

When no signature headers are present at all:

```json
{
  "...": "...",
  "auth_required": true,
  "auth_methods": [],
  "auth_result": "failed"
}
```

### Context Passing Mechanism

Since `http.Request.WithContext()` creates a new request (immutable), and the middleware only has access to the original `r`, we use the **response writer wrapper pattern** already in use:

Extend `responseCapture` (or create a parallel struct) to hold `*AuthResult`. The handler writes to it. The middleware reads from it after ServeHTTP returns.

```go
type responseCapture struct {
    http.ResponseWriter
    statusCode int
    authResult *AuthResult // set by webhook handler
}
```

The webhook handler accesses it via a helper:

```go
func SetAuthOnResponse(w http.ResponseWriter, result *AuthResult) {
    if rc, ok := w.(*responseCapture); ok {
        rc.authResult = result
    }
}
```

### Files Modified

1. `internal/server/middleware.go` — extend `responseCapture`, extend attrs for auth
2. `internal/webhook/forgejo.go` — modify `authenticate()` to return `AuthResult`, call `SetAuthOnResponse`
3. `internal/webhook/authcontext.go` — new file, types and helpers

## Implementation Checklist

- [x] Create `internal/webhook/authcontext.go` with `AuthAttempt`, `AuthResult` types and `SetAuthOnResponse` helper
- [x] Extend `responseCapture` in `internal/server/middleware.go` to hold `*AuthResult`
- [x] Modify `authenticate()` to collect and return `AuthResult`
- [x] Modify `findAndAuthenticateJob()` to propagate auth result to response writer
- [x] Modify `HandleForgejo()` and `HandleSchedule()` to pass response writer to auth chain
- [x] Extend `accessLogMiddleware` to append auth fields when present
- [x] Update tests in `internal/webhook/forgejo_test.go` for new `authenticate()` signature
- [x] Add test for middleware auth field enrichment

## Resolved Questions

- **Only log headers that are actually present (non-empty).** An empty `auth_methods: []` already signals "no signatures sent". Logging all possible methods as "not present" would be noisy.
- **Never log raw signature values.** The `verified: true/false` flag per method is sufficient for diagnostics. Raw HMAC values in logs provide no additional actionable information and increase attack surface.

## Out of Scope

- Changing the auth logic itself (which headers to check, priority order)
- Adding new auth methods
- Webhook secret generation (separate spec)
- Rate limiting or brute-force protection on auth failures
