# AGENTS.md — internal/imageupdate

Polls a container registry to detect updates for the Renovate runner image. Publishes a single `Status` snapshot consumed by `/api/v1/image-status` and rendered as the ⚠ badge in the UI header.

## v1 scope
- **Anonymous polling only.** Bearer-token challenges from the registry are handled registry-agnostically (any realm the registry declares), but no static credentials are consulted:
  - No `~/.docker/config.json` static auths.
  - No `credHelpers` / `credsStore` helpers.
  - No `ROP_REGISTRY_USERNAME/PASSWORD` env vars.
  - No IMDS / workload-identity.
  Private registries need a follow-up plan — surface a GH issue.

## Package structure (planned)
- `ref.go` — image reference parser. ✅ Implemented.
- `ref_test.go` — parser tests. ✅ Implemented.
- `registry.go` — OCI Registry v2 HEAD/GET client. ⏳ Pending (task 7).
- `auth.go` — bearer-token challenge handler. ⏳ Pending (task 7).
- `poller.go` — the tick loop + `Status` state. ⏳ Pending (task 7).
- `poller_test.go` — tick + concurrency tests. ⏳ Pending (task 7).

## Threading & state (planned)
- `Poller` holds an internal `state Status` guarded by `sync.RWMutex`.
- `Snapshot()` returns a **value copy** — handlers never see a shared pointer, no race with a concurrent `Tick`.
- `Tick(ctx)` runs single-threaded (driven by `supervise.Loop` from `cmd/main.go`). It does network I/O without holding the lock, builds a fresh `next Status`, then swaps under the write lock at the end.
- `Ready` is one-way: once flipped true (via first successful tick or `MarkDisabled`), it never flips back. A subsequent tick failure sets `LastError` but keeps `Ready=true`.

## Parse
`Parse(ref string) (Reference, error)` normalizes an OCI reference string:
- Docker Hub bare names (`nginx`) are expanded to `library/nginx`.
- Default registry is `index.docker.io` unless the first segment contains `.`, `:`, or equals `localhost`.
- `@sha256:...` split first (digest); then `:tag` (rightmost `:` after last `/`). Empty tag+empty digest → tag defaults to `latest`.
- Does not validate character legality — the registry will surface invalid characters as 400s.

## What NOT to do
- Do not add credential lookups without a plan. The "anonymous only" contract is explicit; opening a credential source silently expands the operator's attack surface.
- Do not persist `Status` to SQLite in v1 — poller is in-memory only. A future plan can add persistence if we want the banner to survive restarts without re-probing.
- Do not couple to `*executor.DockerExecutor` directly — the poller depends on a narrow `LocalDigester` interface (planned in task 7). Keeps the package testable and swappable.
