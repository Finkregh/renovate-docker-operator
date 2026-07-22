# check=skip=SecretsUsedInArgOrEnv
# ↑ These are empty-string placeholders for runtime injection (via -e / compose env).
#   No actual secrets are baked into the image.

# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build static binary
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.Version=${VERSION}" -o /renovate-docker-operator ./cmd/

# JS dependency download stage
FROM node:24-alpine AS js-downloader
WORKDIR /workspace
RUN apk add --no-cache curl
RUN mkdir -p static/js && \
    curl -s -L -o static/js/tailwind.min.js "https://cdn.tailwindcss.com" && \
    curl -s -L -o static/js/babel.min.js "https://unpkg.com/@babel/standalone@8.0.1/babel.min.js"
RUN mkdir -p /bundle && \
    npm install --prefix /bundle "react@19.2.8" "react-dom@19.2.8" esbuild --save=false && \
    echo "import React from 'react'; import { createRoot } from 'react-dom/client'; export { React, createRoot };" \
        > /bundle/entry.mjs && \
    /bundle/node_modules/.bin/esbuild /bundle/entry.mjs \
        --bundle --format=esm --log-level=silent \
        --outfile=static/js/react-bundle.esm.js

# Runtime stage
FROM alpine:3.24

RUN apk --no-cache add ca-certificates tzdata \
    && addgroup -g 12021 -S operator \
    && adduser -u 12021 -S -G operator -H operator

# Create data directory for SQLite
RUN mkdir -p /data && chown operator:operator /data

WORKDIR /app

COPY --from=builder /renovate-docker-operator /app/renovate-docker-operator

# Copy static UI assets (HTML, components, CSS, pages)
COPY --chown=operator:operator static/ /app/static/

# Overlay downloaded JS dependencies (tailwind, babel, react bundle)
COPY --from=js-downloader /workspace/static/js /app/static/js

# Environment variable defaults (operator config — visible via `docker inspect`)
# Required (no default):
ENV ROP_PLATFORM_ENDPOINT=""
ENV RENOVATE_TOKEN=""

# Platform
ENV RENOVATE_PLATFORM="forgejo"
ENV ROP_IMAGE="renovate/renovate:latest"

# Scheduling
ENV ROP_CRON_SCHEDULE="0 */4 * * *"
ENV ROP_PARALLELISM="2"

# Server
ENV ROP_SERVER_PORT="8081"
ENV ROP_LOG_LEVEL="info"
ENV ROP_CONTAINER_NETWORK=""

# Webhook
ENV ROP_WEBHOOK_ENABLED="true"
ENV ROP_WEBHOOK_SECRET=""

# Auth (leave blank for no-auth mode)
ENV ROP_OIDC_ISSUER_URL=""
ENV ROP_OIDC_CLIENT_ID=""
ENV ROP_OIDC_CLIENT_SECRET=""
ENV ROP_OIDC_REDIRECT_URL=""
ENV ROP_SESSION_SECRET=""

# Discovery
ENV ROP_DISCOVERY_FILTERS=""
ENV ROP_DISCOVER_TOPICS=""
ENV ROP_SKIP_FORKS="false"

# Data
ENV ROP_SQLITE_PATH="/data/renovate.db"
ENV ROP_CACHE_VOLUME="renovate-cache"
ENV ROP_IMAGE_PULL_POLICY="if-not-present"

USER operator

EXPOSE 8081
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=5 \
    CMD sh -c 'curl -f --max-time 2 "http://localhost:8081/healthz" || exit 1'

ENTRYPOINT ["/app/renovate-docker-operator"]
