# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /renovate-docker-operator ./cmd/

# Runtime stage
FROM alpine:3.21

RUN apk --no-cache add ca-certificates tzdata \
    && addgroup -g 12021 -S operator \
    && adduser -u 12021 -S -G operator -H operator

# Create data directory for SQLite
RUN mkdir -p /data && chown operator:operator /data

WORKDIR /app

COPY --from=builder /renovate-docker-operator /app/renovate-docker-operator

# Copy static UI assets (if present in the build context)
COPY --chown=operator:operator static/ /app/static/

USER operator

EXPOSE 8081

ENTRYPOINT ["/app/renovate-docker-operator"]
