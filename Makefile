# Elytra Makefile

.PHONY: help build test clean download-rustic release dev docker

# Default target
help: ## Show this help message
	@echo "Elytra Build System"
	@echo ""
	@echo "Available targets:"
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-15s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# Variables
VERSION ?= $(shell git describe --tags --always --dirty)
BINARY_NAME = elytra
BUILD_DIR = build
GO_VERSION = 1.24.1

# Build configuration
LDFLAGS = -s -w -X github.com/pyrohost/elytra/src/system.Version=$(VERSION)
BUILD_FLAGS = -v -trimpath -ldflags="$(LDFLAGS)"

build: generate ## Build binary for current platform
	@echo "Building $(BINARY_NAME) $(VERSION)..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./src/cmd/elytra

build-all: generate ## Build binaries for all supported platforms
	@echo "Building $(BINARY_NAME) $(VERSION) for all platforms..."
	@mkdir -p $(BUILD_DIR)
	@for GOOS in linux; do \
		for GOARCH in amd64 arm64; do \
			echo "Building $${GOOS}/$${GOARCH}..."; \
			env GOOS=$${GOOS} GOARCH=$${GOARCH} CGO_ENABLED=0 go build $(BUILD_FLAGS) \
				-o $(BUILD_DIR)/$(BINARY_NAME)_$${GOOS}_$${GOARCH} ./src/cmd/elytra; \
		done \
	done
	@echo "Build complete!"

test: generate ## Run tests
	@echo "Running tests..."
	go test -race -cover ./...

debug: generate ## Build and run in debug mode
	go build -ldflags="-X github.com/pyrohost/elytra/src/system.Version=$(VERSION)" -o elytra ./src/cmd/elytra
	sudo ./elytra --debug --ignore-certificate-errors --config config.yml --pprof --pprof-block-rate 1

# Runs a remotely debuggable session for Elytra allowing an IDE to connect and target
# different breakpoints.
rmdebug: generate ## Run with remote debugger
	go build -gcflags "all=-N -l" -ldflags="-X github.com/pyrohost/elytra/src/system.Version=$(VERSION)" -race -o elytra ./src/cmd/elytra
	sudo dlv --listen=:2345 --headless=true --api-version=2 --accept-multiclient exec ./elytra -- --debug --ignore-certificate-errors --config config.yml

release: ## Create a new release (interactive)
	@echo "Creating a new release..."
	./scripts/release.sh

dev: build ## Build and run in development mode
	@echo "Starting $(BINARY_NAME) in development mode..."
	./$(BUILD_DIR)/$(BINARY_NAME) --help

docker: ## Build Docker image
	@echo "Building Docker image..."
	docker build -t ghcr.io/pyrohost/elytra:dev --build-arg VERSION=$(VERSION) .

fmt: ## Format Go code
	@echo "Formatting Go code..."
	go fmt ./...

lint: ## Run linters
	@echo "Running linters..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not found, running go vet instead..."; \
		go vet ./...; \
	fi

deps: ## Download dependencies
	@echo "Downloading dependencies..."
	go mod download
	go mod tidy

# Show build info
info: ## Show build information
	@echo "Build Information:"
	@echo "  Version:     $(VERSION)"
	@echo "  Binary:      $(BINARY_NAME)"
	@echo "  Build Dir:   $(BUILD_DIR)"
	@echo "  Go Version:  $(shell go version)"
	@echo "  Git Commit:  $(shell git rev-parse HEAD)"
	@echo "  Git Branch:  $(shell git rev-parse --abbrev-ref HEAD)"

# Download rustic binaries for embedding
download-rustic:
	@echo "Downloading rustic binaries for embedding..."
	./scripts/download-rustic.sh

# Generate embedded assets
generate: download-rustic
	@echo "Generating embedded assets..."
	go generate ./src/internal/rustic/...

clean: ## Clean build artifacts
	@echo "Cleaning build artifacts..."
	rm -rf $(BUILD_DIR)/*
	rm -rf src/internal/rustic/binaries/rustic-*

.PHONY: help build build-all test debug rmdebug release dev docker fmt lint deps info download-rustic generate clean