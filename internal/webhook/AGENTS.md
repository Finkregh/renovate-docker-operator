# AGENTS.md — internal/webhook

Handles inbound webhooks: `POST /webhook/v1/forgejo` (Forgejo/Gitea events) and `POST /webhook/v1/schedule` (manual, query-string driven). Runs behind the server middleware chain; produces `AuthResult` for the access log.

## Auth is multi-method — ALL are tried
`authenticate()` walks **every** supported signature header present on the request and returns success if **any** matches. The `AuthResult.Methods` list records each attempt (`Type`, `Verified`) — this is emitted by the access log middleware, so operator ops can see which method actually authenticated.

Supported methods, in order tried:
1. `X-Forgejo-Signature` — raw hex HMAC-SHA256 of the body.
2. `X-Hub-Signature-256` — GitHub-style `sha256=<hex>` (prefix stripped before compare).
3. Standard Webhooks (`webhook-id`, `webhook-timestamp`, `webhook-signature`) via `IsWebhookStandardSignatureValid`. Timestamp is bound-checked (`standardWebhookTimestampTolerance = 5m` in statestore) to prevent replay.
4. `X-Webhook-Token` — bearer-style plaintext token compared against `IsWebhookTokenValid`.

Auth **fails closed** — no header, no auth. If a job has `webhook_auth_enabled=false`, auth is skipped entirely and `AuthResult.Required=false`.

## Job resolution is non-trivial
`findAndAuthenticateJob(jobName, project, ...)` walks every RenovateJob and filters via `filterCandidates(jobs, jobName, project)`:
- If `jobName` is set (query string or Standard Webhooks `webhook-source` header) → filter by name.
- Otherwise → find every job whose `projects` list contains `project`.
- If **more than one** job matches, we cannot pick — the caller must supply an explicit `job=` query param. Return an error the resolver translates to 400.

`handleResolverError` maps resolver errors to HTTP codes — a job not found → 404, ambiguous → 400, storage error → 500. Do not surface raw errors to the client.

## Forgejo event filtering
`isValidForgejoEvent(event, payload)` rejects events we don't act on. Currently:
- `issue_comment` is accepted only if the comment body **has a Renovate-toggle checkbox that was just checked** (`hasCheckboxBeenChecked`).
- `issues` is accepted only when the issue description changed in a way that matters (`verifyRenovateDescriptionChange`, checked via `isRenovateContent` on the description).
- `push` / `pull_request` / `pull_request_review` are accepted broadly.

Anything else → 204 (accepted but no-op). This is by design: we want Renovate to keep webhook subscriptions broad, but only trigger runs when needed. Adding a new event? Update `isValidForgejoEvent` and add tests for both the accepted and no-op paths.

## Body size limit
`Handler` receives `maxRequestBody` from `New()` (plumbed from `main.go`). The handler wraps every read with `http.MaxBytesReader`. `isMaxBytesError(err)` detects the sentinel and converts to **413 Payload Too Large**. Do not `io.ReadAll` without this wrapper.

## Auth-context helpers (`authcontext.go`)
- `AuthAttempt` / `AuthResult` are recorded on the response writer via `SetAuthOnResponse(w, result)`. The server's `responseCapture` implements `authResultSetter` — this is how the access log gets enriched. If you write a webhook handler that bypasses this, the log will show `auth_required=false`.

## Resilience wiring
`Handler.SetResilience(mgr)` is optional. When wired, the handler consults the manager to short-circuit dispatch when the breaker is open (returns 503 with retry-after semantics from the manager). Nil-safe: without a manager, dispatch always proceeds.

## Standard Webhooks quirks
- `webhook-id` is the message ID, used for dedup. Currently not deduped in code — SQLite writes are idempotent by project/status, but if you add exactly-once semantics do it here.
- `webhook-timestamp` is a Unix seconds string. Anything more than 5 minutes out of sync → rejected.
- `webhook-signature` is `v1,<base64>` — the store validates it via constant-time comparison.

## HandleSchedule
The manual scheduling endpoint accepts `?project=X&job=Y` and requires the same auth as `/forgejo`. This is the operator's own "kick off a run" webhook, used by the UI and CI.

## Tests
`forgejo_test.go` (large) drives the handler end-to-end with `httptest.NewRecorder`. Fake stores implement `RenovateJobManager` inline for each test — no shared mock. Assertions are typically on status code + `AuthResult` shape.

## Don't
- Don't add a "first match wins" short-circuit to auth — we intentionally verify every present header so `AuthResult.Methods` is complete for logging.
- Don't `io.ReadAll(r.Body)` without wrapping in `http.MaxBytesReader`.
- Don't use `==` or `bytes.Equal` on signatures — always `hmac.Equal` / `subtle.ConstantTimeCompare`.
- Don't reject unknown Forgejo events with 4xx — return 204 so the sender doesn't retry-storm us.
