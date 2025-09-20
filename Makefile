GIT_HEAD = $(shell git rev-parse HEAD | head -c8)

build: generate
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(pwd)" -o build/elytra_linux_amd64 -v ./src/cmd/elytra
	GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(pwd)" -o build/elytra_linux_arm64 -v ./src/cmd/elytra

debug:
	go build -ldflags="-X github.com/pyrohost/elytra/src/system.Version=$(GIT_HEAD)" -o elytra ./src/cmd/elytra
	sudo ./elytra --debug --ignore-certificate-errors --config config.yml --pprof --pprof-block-rate 1

# Runs a remotly debuggable session for Elytra allowing an IDE to connect and target
# different breakpoints.
rmdebug:
	go build -gcflags "all=-N -l" -ldflags="-X github.com/pyrohost/elytra/src/system.Version=$(GIT_HEAD)" -race -o elytra ./src/cmd/elytra
	sudo dlv --listen=:2345 --headless=true --api-version=2 --accept-multiclient exec ./elytra -- --debug --ignore-certificate-errors --config config.yml

cross-build: clean build compress

# Download rustic binaries for embedding
download-rustic:
	@echo "Downloading rustic binaries for embedding..."
	./scripts/download-rustic.sh

# Generate embedded assets
generate: download-rustic
	@echo "Generating embedded assets..."
	go generate ./src/internal/rustic/...

clean:
	rm -rf build/elytra_*
	rm -rf src/internal/rustic/binaries/rustic-*

.PHONY: all build compress clean download-rustic generate