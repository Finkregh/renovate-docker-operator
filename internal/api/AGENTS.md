# AGENTS.md — internal/api

## Purpose
Pure type-definition package: shared DTOs serialized to JSON for the HTTP API **and** the React web UI.
No business logic, no handlers, no database access — only structs, enums, and trivial helper methods.

## Historical Context
Types are derived from `mogenius/renovate-operator api/v1alpha1` (Kubernetes CRD) with **all K8s dependencies removed**.
`EnvVar` replaces `corev1.EnvVar`; there is no `ObjectMeta`/`TypeMeta` — the standalone `RenovateJob` struct uses a plain `Name` field.

## What Belongs Here
- Request/response structs consumed by `internal/server` and the web UI
- Enum-style `const` blocks (e.g. `RenovateProjectStatus`, `PRAction`)
- Small value-object helpers (e.g. `Fullname()`)

## What Does NOT Belong
- HTTP handlers, middleware, routing
- Persistence/SQL logic (that's `internal/statestore`)
- Business rules, scheduling, orchestration

## JSON Tag Conventions
- All fields use `json:"camelCase"` (not snake_case)
- Optional/zero-value fields carry `,omitempty`
- Pointer types (`*T`) are used for truly nullable fields (absence ≠ zero value)

## Gotchas
- These types are **persisted** by `statestore` (serialized as JSON blobs in SQLite). Renaming a JSON tag is a breaking migration.
- The web UI (React/JSX in `static/`) consumes these exact JSON shapes — any field rename or removal also breaks the frontend.
- `PRActivity.NeedsApproval` uses a capital `N` in Go but `"needsApproval"` in JSON — the JSON key does **not** match the `PRAction` constant `"needs-approval"` (hyphenated). Be careful when comparing.
- `time.Time` fields (e.g. `LastRun`) serialize as RFC 3339 strings; the frontend expects this format.
- `Parallelism` is `int32` (not `int`) — matches the original K8s CRD convention.

## Versioning
No formal API versioning (no `/v1/` prefix or version header). Breaking changes must be coordinated across `server`, `statestore`, and `static/` simultaneously.
