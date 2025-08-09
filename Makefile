GIT_HEAD = $(shell git rev-parse HEAD | head -c8)
VERSION ?= $(GIT_HEAD)

# Build targets
build:
	@mkdir -p build
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X github.com/pyrohost/elytra/src/system.Version=$(VERSION)" -trimpath -o build/elytra_linux_amd64 -v src/elytra.go
	GOOS=linux GOARCH=arm64 go build -ldflags="-s -w -X github.com/pyrohost/elytra/src/system.Version=$(VERSION)" -trimpath -o build/elytra_linux_arm64 -v src/elytra.go

# Development build
dev:
	go build -ldflags="-X github.com/pyrohost/elytra/src/system.Version=$(VERSION)" -o elytra src/elytra.go

# Debug build with profiling
debug: dev
	sudo ./elytra --debug --ignore-certificate-errors --config config.yml --pprof --pprof-block-rate 1

# Remote debugging session for IDE integration
rmdebug:
	go build -gcflags "all=-N -l" -ldflags="-X github.com/pyrohost/elytra/src/system.Version=$(VERSION)" -race -o elytra src/elytra.go
	sudo dlv --listen=:2345 --headless=true --api-version=2 --accept-multiclient exec ./elytra -- --debug --ignore-certificate-errors --config config.yml

# Test the application
test:
	go test -v ./src/...

# Format code
fmt:
	go fmt ./src/...

# Lint code
lint:
	golangci-lint run ./src/...

# Clean build artifacts
clean:
	rm -rf build/
	rm -f elytra

# Install dependencies
deps:
	go mod download
	go mod tidy

# Cross-platform build
cross-build: clean build

.PHONY: build dev debug rmdebug test fmt lint clean deps cross-build