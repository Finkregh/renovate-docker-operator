export CGO_ENABLED := "0"

set dotenv-load

[private]
default:
    just --list --unsorted

# Run the application with flags similar to the production build
run *args: build jsInstall
    ./dist/native/renovate-operator {{args}}

# Build a native binary with flags similar to the production build
build:
    #!/usr/bin/env sh
    VERSION=$(git describe --tags $(git rev-list --tags --max-count=1) 2>/dev/null || echo "dev")
    go build -tags timetzdata -trimpath -gcflags="all=-l" -ldflags="-s -w -X main.Version=${VERSION}" -o dist/native/renovate-operator ./cmd/main.go

# Build binaries for all targets
build-all: build-linux-amd64 build-linux-arm64 build-linux-armv7

# Build binary for target linux-amd64
build-linux-amd64:
    #!/usr/bin/env sh
    VERSION=$(git describe --tags $(git rev-list --tags --max-count=1) 2>/dev/null || echo "dev")
    GOOS=linux GOARCH=amd64 go build -tags timetzdata -trimpath -gcflags="all=-l" -ldflags="-s -w -X main.Version=${VERSION}" -o dist/amd64/renovate-operator ./cmd/main.go

# Build binary for target linux-arm64
build-linux-arm64:
    #!/usr/bin/env sh
    VERSION=$(git describe --tags $(git rev-list --tags --max-count=1) 2>/dev/null || echo "dev")
    GOOS=linux GOARCH=arm64 go build -tags timetzdata -trimpath -gcflags="all=-l" -ldflags="-s -w -X main.Version=${VERSION}" -o dist/arm64/renovate-operator ./cmd/main.go

# Build binary for target linux-armv7
build-linux-armv7:
    #!/usr/bin/env sh
    VERSION=$(git describe --tags $(git rev-list --tags --max-count=1) 2>/dev/null || echo "dev")
    GOOS=linux GOARCH=arm GOARM=7 go build -tags timetzdata -trimpath -gcflags="all=-l" -ldflags="-s -w -X main.Version=${VERSION}" -o dist/armv7/renovate-operator ./cmd/main.go

# Run tests and linters for quick iteration locally.
check: golangci-lint test-unit

# Execute unit tests
test-unit:
    go run gotest.tools/gotestsum@latest --format="testname" --hide-summary="skipped" --format-hide-empty-pkg --rerun-fails="0" -- -count=1 ./...

# Execute golangci-lint
golangci-lint:
    go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run --config=.golangci.yml '--timeout=1h' ./...

# Install JS dependencies
jsInstall:
    #!/usr/bin/env sh
    set -e
    mkdir -p static/js
    echo "Downloading Tailwind CSS..."
    curl -s -L -o static/js/tailwind.min.js "https://cdn.tailwindcss.com"
    echo "Downloading Babel Standalone..."
    curl -s -L -o static/js/babel.min.js "https://unpkg.com/@babel/standalone@8.0.1/babel.min.js"
    echo "Bundling React 19..."
    BUNDLE_DIR=$(mktemp -d)
    npm install --prefix "$BUNDLE_DIR" "react@19.2.7" "react-dom@19.2.7" esbuild --save=false --silent
    echo "import React from 'react'; import { createRoot } from 'react-dom/client'; export { React, createRoot };" > "$BUNDLE_DIR/entry.mjs"
    "$BUNDLE_DIR/node_modules/.bin/esbuild" "$BUNDLE_DIR/entry.mjs" \
        --bundle --format=esm --log-level=silent \
        --outfile=static/js/react-bundle.esm.js
    rm -rf "$BUNDLE_DIR"
    echo "All JavaScript dependencies ready!"

# Build and push container image
docker-push image="git.h.oluflorenzen.de/finkregh/renovate-docker-operator":
    podman build --platform linux/arm64 \
        -t {{image}}-arm64 \
        -f ./Dockerfile .
    @echo "Creating manifest..."
    podman manifest rm {{image}} 2>/dev/null || true
    podman manifest create {{image}}
    @echo "Adding ARM64 to manifest..."
    podman manifest add {{image}} {{image}}-arm64
    @echo "Inspecting manifest:"
    podman manifest inspect {{image}}
    @echo "Pushing manifest:"
    podman manifest push --all {{image}}
