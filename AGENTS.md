# AGENTS.md

## Setup Commands

- Build: `just build` (native binary → `dist/native/renovate-operator`)
- Run: `just run` (builds first, requires JS deps)
- Install JS deps: `just jsInstall` (downloads Tailwind, Babel, bundles React 19 via esbuild into `static/js/`)
- Run tests: `just test-unit` (uses `gotestsum`)
- Lint: `just golangci-lint` (runs `golangci-lint v2` with `.golangci.yml`)
- Quick check (lint + test): `just check`
- Cross-compile: `just build-all` (amd64, arm64, armv7)
- Container build+push: `just docker-push`

> Note: `CGO_ENABLED=0` is exported globally in the Justfile.

## Code Style

- **Go 1.26.5** — pure-Go, no CGO
- Format with `gofmt`; lint with golangci-lint v2
- Enabled linters: errcheck, govet, ineffassign, staticcheck, unused, bodyclose, durationcheck, errname, errorlint, fatcontext, gocritic, gocyclo, misspell, noctx, nilerr, prealloc, revive, unconvert, unparam, usestdlibvars
- Lint timeout: 10 min (CI uses 1 h)

## Project Conventions

- **Module path:** `git.h.oluflorenzen.de/finkregh/renovate-docker-operator`
- **Entrypoint:** `cmd/main.go`
- **Internal packages:** `internal/<pkg>/` — each has its own `AGENTS.md` with package-specific guidance
- **Web UI:** `static/` — React 19 JSX compiled client-side via Babel, styled with Tailwind; JS deps are NOT checked in (downloaded via `just jsInstall`)
- **Config:** `config/` — runtime configuration structs
- **Conventional Commits** enforced by `cog.toml` — types: feat, fix, docs, chore, refactor, test, perf, ci, build, revert, ui
- **DCO required** — all commits need `Signed-off-by` trailer (`git commit --signoff`)
- **AI co-authorship** — add `Co-Authored-By` footer when AI-assisted
- **Versioning:** `cog` bumps from latest tag, pushes tags, creates Forgejo release
- **Branch names:** `<type>/<short-description>` (e.g. `feat/parallel-execution`)

## Testing

- Run: `just test-unit` (wraps `gotestsum --format=testname`)
- Style: table-driven tests, `_test.go` files beside source
- Coverage: no minimum enforced, but new features should include tests
- CI: `.github/workflows/go-test.yaml` runs tests on PR

## Security

- **Never commit secrets** — `.env.example` documents all vars; actual `.env` is gitignored
- Secrets are injected via environment variables at runtime
- Webhook HMAC secret is auto-generated on first startup (stored in SQLite)
- Dockerfile uses empty-string ARG placeholders — no secrets baked into images
- CSRF protection via Origin header checking
- Runs as non-root user (UID 12021) in container

## Common Gotchas

- **Must run `just jsInstall` before `just run`** — `static/js/` is gitignored and not checked in; the server will fail to serve the UI without it
- **CGO is disabled globally** (`export CGO_ENABLED := "0"` in Justfile) — SQLite uses the pure-Go `modernc.org/sqlite` driver, not `mattn/go-sqlite3`
- **Conventional commits are enforced** — CI rejects non-conforming commit messages; use `cog check` locally to verify
- **Docker socket required at runtime** — the operator manages Renovate containers directly via `/var/run/docker.sock`
- **`just build` embeds version from latest git tag** — if no tags exist, version is `"dev"`
- **`-count=1` in test command** — test caching is intentionally disabled
- **golangci-lint is run via `go run`** — no need to install it globally
