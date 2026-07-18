# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /renovate-docker-operator ./cmd/

# JS dependency download stage
FROM node:24-alpine AS js-downloader
WORKDIR /workspace
RUN apk add --no-cache curl
RUN mkdir -p static/js && \
    curl -s -L -o static/js/tailwind.min.js "https://cdn.tailwindcss.com" && \
    curl -s -L -o static/js/babel.min.js "https://unpkg.com/@babel/standalone@8.0.1/babel.min.js"
RUN mkdir -p /bundle && \
    npm install --prefix /bundle "react@19.2.7" "react-dom@19.2.7" esbuild --save=false && \
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
ENV PLATFORM_ENDPOINT=""
ENV RENOVATEOP_TOKEN=""

# Platform
ENV PLATFORM="forgejo"
ENV RENOVATEOP_IMAGE="renovate/renovate:latest"

# Scheduling
ENV CRON_SCHEDULE="0 */4 * * *"
ENV GLOBAL_PARALLELISM_LIMIT="2"

# Server
ENV SERVER_PORT="8081"
ENV RENOVATEOP_LOG_LEVEL="info"
ENV CONTAINER_NETWORK=""

# Webhook
ENV WEBHOOK_SERVER_ENABLED="true"
ENV WEBHOOK_SECRET=""

# Auth (leave blank for no-auth mode)
ENV OIDC_ISSUER_URL=""
ENV OIDC_CLIENT_ID=""
ENV OIDC_CLIENT_SECRET=""
ENV OIDC_REDIRECT_URL=""
ENV SESSION_SECRET=""

# Discovery
ENV RENOVATEOP_DISCOVERY_FILTERS=""
ENV RENOVATEOP_DISCOVER_TOPICS=""
ENV AUTODISCOVER_SKIP_FORKS="false"

# Data
ENV SQLITE_PATH="/data/renovate.db"
ENV CACHE_VOLUME="renovate-cache"
ENV IMAGE_PULL_POLICY="if-not-present"

USER operator

EXPOSE 8081
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=5 \
    CMD sh -c 'curl -f --max-time 2 "http://localhost:8081/healthz" || exit 1'

ENTRYPOINT ["/app/renovate-docker-operator"]
