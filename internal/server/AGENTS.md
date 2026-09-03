# AGENTS.md — internal/server

The unified HTTP server: health, API v1, webhooks, `/metrics`, and the static React UI. Uses `github.com/gorilla/mux`.

## Router shape (order matters)
`Start()` builds one router with this exact order — do **not** reorder, the UI catch-all must be last:
1. `/healthz`, `/readyz` — health probes (no auth, no CSRF).
2. `/webhook/v1/*` — `/forgejo`, `/schedule` (subrouter, POST only).
3. `/api/v1/*` — the operator API (subrouter). Every route uses `.Methods("...")` so wrong methods return 405, not 404.
4. `/metrics` — Prometheus, **no auth** (per spec §7; scraping is deployment-controlled).
5. `s.registerUIRoutes(router)` — installs a `PathPrefix("/")` catch-all serving `./static/`. Must be last.

## Non-obvious wiring
- `New()` returns a Server without resilience or metrics wired. `main.go` calls `SetResilience` and `SetMetrics` after construction. Both are nil-safe:
  - `/api/v1/breaker/*` returns **501** if `resilience == nil`.
  - `/metrics` returns 501 if `metrics == nil`.
  - `SetResilience` also propagates the manager into the webhook handler (`s.webhook.SetResilience(mgr)`).
- The webhook handler is constructed inline in `New()` — it is **not** passed in. If you refactor, keep the `maxRequestBody` limit propagation.

## Middleware chain
`accessLogMiddleware → csrfMiddleware → router`. Both are defined in `middleware.go`, not from a third-party lib:
- **`accessLogMiddleware`** — logs every request; `/healthz` and `/readyz` are demoted to `debug` level to avoid probe noise. Uses `responseCapture` to catch status codes and (for webhooks) an `AuthResult` to enrich the log line with `auth_required`, `auth_methods`, `auth_result`. `responseCapture` implements `Unwrap()` so `http.ResponseController` still works through the wrapper.
- **`csrfMiddleware`** — Origin-header check on POST/PUT/DELETE **only**. If `Origin` is present and its host ≠ `r.Host`, reject 403. If `Origin` is absent (curl, webhooks, non-browser clients) the request is allowed. This means CSRF protection is browser-only by design; server-to-server callers just don't send Origin.

## UI static serving (`registerUIRoutes`)
- Serves from `./static/` **on disk** (not `embed.FS`) — the operator process must be started from a directory where `./static` exists (Justfile's `just run` does this; `just jsInstall` builds it). If the dir is missing, UI routes are simply not registered and `/api/*` still works.
- Two hardcoded HTML routes: `/` and `/logs` (plus their `.html` variants) are served via `serveHTML()` which sets `Cache-Control: no-cache`. Everything else falls through to `http.FileServer`.

## Server timeouts (hardcoded)
- `ReadTimeout: 30s`, `WriteTimeout: 60s`, `IdleTimeout: 120s`.
- These are shared across all endpoints — `WriteTimeout=60s` limits how long the SSE-style `GET /api/v1/logs/live` can stream. If you extend that endpoint, revisit timeouts (or wrap it in `http.ResponseController.SetWriteDeadline`).
- Port: `ROP_SERVER_PORT` env var (default `8081`), read via `config.GetValue`.

## Request body limit
`maxRequestBody` (bytes) is enforced by the webhook handler via `http.MaxBytesReader`. It is passed from `main.go`. The `isMaxBytesError` helper (in webhook) is used to convert those errors into 413 responses. API endpoints don't currently enforce a size limit — add one via `MaxBytesReader` in a middleware if a new endpoint needs one.

## Auth model
- API endpoints have **no auth**; the operator is expected to sit behind an authenticated reverse proxy.
- Webhook endpoints do their own signature/token auth inside `webhook.Handler` — the server middleware just captures the outcome for logging.
- `actorFromRequest` derives a Basic-Auth username from the request purely for **audit logging** on breaker reset/bypass — it is **not** an auth check.

## Breaker endpoints
- `GET /api/v1/breaker/state` — returns `resilience.Manager.Snapshot()`.
- `POST /api/v1/breaker/reset` — resets the global breaker; audited with `actorFromRequest`.
- `POST /api/v1/breaker/bypass/{project:.+}` — the `{project:.+}` regex is required because project names contain `/` (e.g. `org/repo`).

## Tests
`server_test.go` and `middleware_test.go` use `httptest.NewRequest`/`httptest.NewRecorder`. There is no test helper for spinning up the full router with all deps — tests construct Server pieces directly. When adding a new handler, table-drive it with recorded responses; don't reach for `httptest.NewServer` unless you truly need a listener.

## Don't
- Don't put `/metrics` behind auth or CSRF — Prometheus scrape.
- Don't reorder middleware to put CSRF before access log — you'll lose the 403 in access logs.
- Don't add per-request Origin allowlists in code — this is intentionally simple (Origin host must equal request Host).
