# renovate-docker-operator

A standalone Docker-based operator for running [Renovate](https://github.com/renovatebot/renovate) against Forgejo/Gitea instances. Manages Renovate containers via Docker socket — no Kubernetes required.

---

## Origin & Motivation

This project is a fork of [**mogenius/renovate-operator**](https://github.com/mogenius/renovate-operator), adapted for environments that don't run Kubernetes.

**Why does this exist?**

1. **Renovate CE/EE does not support Forgejo.** The feature request ([mend-io/renovate-ce-ee#173](https://github.com/mend-io/renovate-ce-ee/issues/173)) was locked and closed without resolution. If you self-host Forgejo, Mend's commercial offering simply won't work.

2. **The upstream renovate-operator is Kubernetes-only.** It relies on CRDs, controller-runtime, and Kubernetes Jobs for container orchestration — a non-starter if your infrastructure is Docker Compose, Podman, or a single VM.

3. **This project extracts the core logic into a standalone binary** that talks directly to the Docker socket. Same features (priority queues, parallelism, webhooks, discovery, UI), zero Kubernetes dependencies, single ~20 MB binary + SQLite for state.

> **Credit:** The Forgejo webhook handling, repository discovery agent, priority queue, and web UI patterns all originate from the excellent work by the [mogenius](https://github.com/mogenius) team. Thank you for building this in the open.

---

## Differences from Upstream

| Aspect | [mogenius/renovate-operator](https://github.com/mogenius/renovate-operator) | renovate-docker-operator |
|--------|-----------------------------------------------------------------------------|--------------------------|
| Runtime | Kubernetes (CRDs, controller-runtime) | Docker (socket API) |
| State store | Kubernetes CRDs + etcd | SQLite (WAL mode) |
| Container orchestration | Kubernetes Jobs | Docker containers |
| Configuration | CRDs + ConfigMaps | Environment variables |
| Image size | ~50 MB (requires K8s cluster) | ~20 MB standalone binary |
| Dependencies | controller-runtime, client-go | Docker SDK, modernc.org/sqlite |
| Auth | OIDC + GitHub OAuth | Same (OIDC + GitHub OAuth + no-auth) |
| Platforms | Forgejo, Gitea, GitHub, GitLab | Same |
| Renovate features | Priority queue, parallelism, webhooks, UI | Same |
| Deployment | Helm chart, K8s cluster | docker-compose or bare binary |

We share the same Forgejo webhook logic, discovery agent, and UI patterns as upstream. This means improvements from [mogenius/renovate-operator](https://github.com/mogenius/renovate-operator) can be cherry-picked into this project when relevant.

---

## Features

- **Docker-native**: No Kubernetes required. Manages Renovate containers via Docker socket.
- **SQLite state**: Lightweight persistence with WAL mode for concurrent access.
- **Webhook-driven**: Forgejo webhooks trigger immediate Renovate runs on PR/issue changes.
- **Cron scheduling**: Automatic discovery and execution on configurable cron schedules.
- **Web UI**: Dashboard showing job status, project runs, and streaming logs.
- **OIDC auth**: Optional authentication via any OIDC provider (Forgejo itself works!).

---

## Quick Start

```bash
# 1. Clone
git clone https://github.com/oluf-tech/renovate-docker-operator.git
cd renovate-docker-operator

# 2. Set required env vars
export RENOVATE_TOKEN="your-forgejo-token"

# 3. Start with Docker Compose
docker compose up -d
```

The UI is available at <http://localhost:8081>

---

## Configuration

All configuration is via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `PLATFORM_ENDPOINT` | *(required)* | Forgejo/Gitea instance URL |
| `RENOVATE_TOKEN` | *(required)* | Platform access token for Renovate |
| `PLATFORM` | `forgejo` | Platform type (`forgejo`, `gitea`, `github`, `gitlab`) |
| `RENOVATE_IMAGE` | `renovate/renovate:latest` | Docker image for Renovate |
| `CRON_SCHEDULE` | `0 */4 * * *` | Cron expression for discovery+run cycles |
| `GLOBAL_PARALLELISM_LIMIT` | `2` | Max concurrent Renovate containers |
| `SERVER_PORT` | `8081` | HTTP server port (UI + webhook + API) |
| `SQLITE_PATH` | `/data/renovate.db` | Path to SQLite database |
| `CACHE_VOLUME` | `renovate-cache` | Docker volume for Renovate cache |
| `CONTAINER_NETWORK` | *(empty)* | Docker network for Renovate containers |
| `IMAGE_PULL_POLICY` | `if-not-present` | When to pull image (`always`, `if-not-present`, `never`) |
| `JOB_TIMEOUT_SECONDS` | `1800` | Max runtime per Renovate container (30 min) |
| `SHUTDOWN_GRACE_PERIOD` | `300` | Grace period for stopping containers on shutdown (5 min) |
| `LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`) |

### Webhook Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `WEBHOOK_SERVER_ENABLED` | `true` | Enable webhook endpoint |
| `WEBHOOK_SECRET` | *(empty)* | Comma-separated HMAC secrets for webhook validation |

Configure your Forgejo instance to send webhooks to:

```
http://renovate-operator:8081/webhook/v1/forgejo?job=default
```

Events to enable: **Issues** (edited) and **Pull Requests** (edited, closed, reopened).

### Authentication (Optional)

| Variable | Default | Description |
|----------|---------|-------------|
| `OIDC_ISSUER_URL` | *(empty)* | OIDC provider URL (leave empty for no-auth) |
| `OIDC_CLIENT_ID` | *(empty)* | OAuth2 client ID |
| `OIDC_CLIENT_SECRET` | *(empty)* | OAuth2 client secret |
| `OIDC_REDIRECT_URL` | *(empty)* | OAuth2 redirect URL |
| `SESSION_SECRET` | *(auto-generated)* | AES key for session cookies |

**Tip**: Use Forgejo itself as your OIDC provider! Forgejo 1.22+ has built-in OAuth2 provider support. Create an OAuth2 application in Forgejo settings and point `OIDC_ISSUER_URL` at your Forgejo instance.

### Discovery Filters

| Variable | Default | Description |
|----------|---------|-------------|
| `RENOVATE_DISCOVERY_FILTERS` | *(empty)* | Comma-separated repo patterns (e.g., `org/*,user/repo-*`) |
| `RENOVATE_DISCOVER_TOPICS` | *(empty)* | Comma-separated topics to filter by |
| `AUTODISCOVER_SKIP_FORKS` | `false` | Skip forked repositories |
| `CRON_SKIP_DISCOVERY` | `false` | Skip discovery on cron (only run known projects) |

---

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/healthz` | Health check |
| `GET` | `/api/v1/version` | Server version |
| `GET` | `/api/v1/renovatejobs` | List all jobs with project statuses |
| `POST` | `/api/v1/renovate` | Trigger Renovate for a project |
| `POST` | `/api/v1/renovate/all` | Trigger all projects |
| `POST` | `/api/v1/renovate/cancel` | Cancel a running project |
| `GET` | `/api/v1/logs?renovate=X&project=Y` | Stream logs (SSE) |
| `POST` | `/api/v1/discovery/start` | Trigger discovery |
| `POST` | `/api/v1/executionOptions` | Update debug mode |
| `POST` | `/webhook/v1/forgejo?job=X` | Forgejo webhook receiver |
| `POST` | `/webhook/v1/schedule?project=X&job=Y` | Manual schedule trigger |

---

## Architecture

```
┌─────────────────────────────────────────┐
│          renovate-docker-operator       │
├─────────────────────────────────────────┤
│  HTTP Server (port 8081)                │
│  ├── /webhook/v1/* (Forgejo hooks)      │
│  ├── /api/v1/*     (REST API)           │
│  ├── /healthz      (health)             │
│  └── /*            (static UI)          │
├─────────────────────────────────────────┤
│  Scheduler (robfig/cron)                │
│  └── Discovery → Schedule → Dispatch    │
├─────────────────────────────────────────┤
│  Docker Executor                        │
│  ├── Container create/start/wait/logs   │
│  ├── Priority queue dispatch            │
│  └── Docker Events API (exit detect)    │
├─────────────────────────────────────────┤
│  SQLite State Store (WAL mode)          │
│  └── Jobs, Projects, Logs, Webhooks     │
└─────────────────────────────────────────┘
         │
         ▼ Docker Socket
┌─────────────────────────────────────────┐
│  renovate/renovate containers           │
│  (one per project, max parallelism)     │
└─────────────────────────────────────────┘
```

---

## Development

```bash
# Build
go build ./cmd/

# Run tests
go test ./...

# Run locally (requires Docker and PLATFORM_ENDPOINT)
export PLATFORM_ENDPOINT="https://git.example.com"
export RENOVATE_TOKEN="your-token"
export SQLITE_PATH="./test.db"
./renovate-docker-operator
```

---

## Related Projects

- [**mogenius/renovate-operator**](https://github.com/mogenius/renovate-operator) — Original Kubernetes-based operator (upstream)
- [**renovatebot/renovate**](https://github.com/renovatebot/renovate) — Renovate itself
- [**mend-io/renovate-ce-ee**](https://github.com/mend-io/renovate-ce-ee) — Official Mend Renovate CE/EE (no Forgejo support)

---

## License

Apache 2.0 — see [LICENSE](LICENSE).
