# AGENTS.md — .github/workflows/

## Platform & Registry
- **Not GitHub.com** — runs on a self-hosted Forgejo instance at `git.h.oluflorenzen.de`.
- Actions use Forgejo-specific checkout (`https://code.forgejo.org/actions/checkout`) and Codeberg-hosted actions.
- Container images push to the Forgejo built-in registry: `git.h.oluflorenzen.de/{owner}/{repo}:{tag}`.
- Binary releases upload to Forgejo Generic Package Registry, not GitHub Releases assets.

## Workflow Triggers
| Workflow | Trigger |
|----------|---------|
| `go-test.yaml` | PR → main |
| `conventional-commits.yaml` | Any PR (all branches) |
| `build-pr-image.yaml` | PR opened / synchronize / reopened |
| `cleanup-pr-image.yaml` | PR closed |
| `build-tagged-release.yaml` | Release published OR `workflow_dispatch` |
| `release-cog.yaml` | PR opened / synchronize / reopened OR `workflow_dispatch` |

## Reusable Workflow: `reusable-build-image.yaml`
- `workflow_call` only — never runs standalone.
- Callers: `build-pr-image.yaml` (tag: `pr-<N>`) and `build-tagged-release.yaml` (tags: version + `latest`).
- Inputs: `tag_suffixes` (newline-separated), `checkout_ref`, `fetch_tags`.
- Uses Docker-in-Docker via `DOCKER_HOST: tcp://docker_dind:2375` with `--privileged` container.
- Layer cache: GitHub Actions cache (`type=gha`, `mode=max`).

## PR Image Lifecycle
1. **Build** (`build-pr-image.yaml`): builds image tagged `pr-<N>`, posts a comment on the PR with the pull command (skips if already commented).
2. **Cleanup** (`cleanup-pr-image.yaml`): on PR close, deletes the `pr-<N>` tag from the container registry via API.

## Conventional Commits Enforcement
- `conventional-commits.yaml` validates **all commit messages** between base and head using a bash regex.
- Allowed types: `feat|fix|docs|chore|refactor|test|perf|ci|build|revert` with optional `(<scope>)` and `!` for breaking.
- This is independent of `cog.toml` — it's a pure regex check, not cocogitto.

## Release: `release-cog.yaml`
- Uses a custom Forgejo action (`finkregh/actions/actions/release-with-cog`) that wraps **cocogitto**.
- Runs on PR events (preview changelog?) AND `workflow_dispatch` (actual release).
- Requires `USER_PUSH_TOKEN` (separate from `PACKAGE_UPLOAD_TOKEN`) with push permission for tagging.

## Go Testing (`go-test.yaml`)
- **No matrix** — single job, single OS (ubuntu-latest in node:24-bookworm container).
- Go version from `go.mod` (`go-version-file`), cached via `setup-go` action's built-in cache.
- Runs: lint → test → build (linux/amd64) sequentially in one job.

## Required Secrets
| Secret | Used by |
|--------|---------|
| `PACKAGE_UPLOAD_TOKEN` | Image push, PR comment, cleanup, binary upload |
| `USER_PUSH_TOKEN` | `release-cog.yaml` — needs repo push (for tags/changelog commits) |

## Non-Obvious Details
- All jobs pin a specific `node:24-bookworm` container image by SHA digest — even non-Node workflows. This is the Forgejo runner's base image requirement.
- Docker is installed at runtime inside the container (apt-get) rather than pre-installed.
- `build-tagged-release.yaml` produces binaries for 3 architectures (amd64, arm64, armv7) but the container image is **single-arch** (runner's native arch via buildx).
- No OIDC/keyless signing is used; auth is purely token-based.
