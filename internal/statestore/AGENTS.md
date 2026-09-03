# AGENTS.md — internal/statestore

SQLite-backed persistence. This is the **standalone (non-Kubernetes)** replacement for the upstream `crdManager`. All operator state — jobs, projects, logs, webhook secrets, settings, discovery status — lives in one SQLite file.

## Non-obvious: pure-Go SQLite driver
- Uses `modernc.org/sqlite` (CGO-free) — registered in `driver.go` via a blank import. This is why `CGO_ENABLED=0` in the Justfile works; **do not** switch to `mattn/go-sqlite3` without unwinding CGO settings project-wide.
- Two `*sql.DB` handles per store:
  - **`writeDB`** — opened with `?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON`, `SetMaxOpenConns(1)`. **Single writer** at the connection level to avoid `SQLITE_BUSY` on write contention.
  - **`readDB`** — same DSN + `mode=ro`. Multiple concurrent readers are safe under WAL.
- Every method must pick the right handle: reads → `readDB`, writes/transactions → `writeDB`. Grep for `readDB` vs `writeDB` before adding a new method.

## Migrations
- `migrations.go` — an **ordered `[]string`** of DDL applied via `runMigrations()`. There are 4 (`migration0001`..`migration0004`). A `schema_migrations` table tracks which have run.
- Add-only: new migrations append to the slice; **never reorder or edit** an existing migration string — production databases already ran them. Version numbering is by slice index.
- Migrations run on **every startup** inside `New()` before anything else touches the DB. Schema changes must be backward compatible with any code paths that still run against the older schema in a rolling deploy.

## Interface contract (`interface.go`)
`RenovateJobManager` is the sole entry point used by `discovery`, `scheduler`, `executor`, `server`, `webhook`. If you change it, every one of those packages breaks — grep before editing.

Notable behaviors codified in comments:
- `ReconcileProjects` returns **removed** project names (so callers can clean up webhooks).
- `UpdateProjectStatusBatched` takes a **predicate** — used to transition many projects at once (e.g. cancel-all).
- `ResetOrphanedDiscoveryStatus` is called on startup to clear stale `running` discoveries that survived a restart.
- `IsWebhookSignatureValid` (Forgejo/GitHub HMAC) vs `IsWebhookStandardSignatureValid` (Standard Webhooks spec) are **separate methods** — the webhook handler tries them in sequence.
- All errors returned as `ErrProjectNotFound` / `ErrNotFound` (sentinel errors from `interface.go`) — do not wrap them so callers can `errors.Is` cleanly.

## Webhook secret handling
- Webhook signing keys are cached in `SQLiteStore.webhookTokens` at `New()` time from the `settings` table.
- If no secret exists, `New()` auto-generates one via `crypto/rand` and persists it in `settings` — first run is not authenticated until the operator writes this row.
- HMAC comparisons use `hmac.Equal` / `subtle.ConstantTimeCompare` (no `==` on secret bytes).
- `standardWebhookTimestampTolerance = 5m` — Standard Webhooks replay window. Requests older than this are rejected.

## Seed data
`seedDefaultJob()` runs at startup: if `renovate_jobs` is empty, a job named from env (`RENOVATE_JOB_NAME`) is inserted with defaults from `RENOVATE_*` env vars. This is the single-tenant deployment path — do not add multi-job seeding here.

## Settings table (`settings.go`)
Generic key/value store for operator-scoped config (webhook secret, execution options, etc.). Not for per-job or per-project state — those get real columns/tables. `SetSetting` upserts via `ON CONFLICT(key) DO UPDATE`.

## Log storage
`StoreProjectLogs` stores the raw log blob per project run. `StreamLogsForProject` returns an `io.ReadCloser` that streams NDJSON — implemented on the read side, so multiple viewers can tail concurrently. Live-tail (`/api/v1/logs/live`) is served by the executor directly against the Docker daemon, not from here.

## Testing pattern
`sqlite_test.go` uses **file-backed** SQLite databases in `t.TempDir()` (WAL + foreign keys need real files; `:memory:` was tried and doesn't behave identically). Each test builds its own `SQLiteStore` — no shared fixtures.

## Don't
- Don't open a new `sql.DB` per request — always use the two-DB handles on `SQLiteStore`.
- Don't `SetMaxOpenConns > 1` on `writeDB` — you'll get busy errors under load.
- Don't touch the `schema_migrations` table from application code.
- Don't compare webhook secrets with `==`.
