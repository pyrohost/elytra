# Stage 1 (Build)
FROM golang:1.24-alpine AS builder

ARG VERSION
RUN apk add --update --no-cache git make mailcap curl bash
WORKDIR /app/
COPY go.mod go.sum /app/
RUN go mod download
COPY . /app/
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X github.com/pyrohost/elytra/src/system.Version=$VERSION" \
    -v \
    -trimpath \
    -o elytra \
    ./src/cmd/elytra
RUN echo "ID=\"distroless\"" > /etc/os-release

# Stage 2 (Rustic prebuilt)
# Download a pre-built rustic binary from the project's GitHub releases instead
# of building it every time. The release version and target triple can be
# overridden at build time with --build-arg RUSTIC_VERSION=... and
# --build-arg RUSTIC_TARGET=...
FROM alpine:3.18 AS rust-builder
ARG RUSTIC_VERSION=v0.10.0
ARG RUSTIC_TARGET=x86_64-unknown-linux-musl
RUN apk add --no-cache ca-certificates curl tar gzip
WORKDIR /tmp
RUN set -eux; \
    file="rustic-${RUSTIC_VERSION}-${RUSTIC_TARGET}.tar.gz"; \
    url="https://github.com/rustic-rs/rustic/releases/download/${RUSTIC_VERSION}/${file}"; \
    echo "Downloading ${url}"; \
    curl -fsSL --retry 3 "$url" -o "$file"; \
    tar -xzf "$file"; \
    p=$(find . -type f -name rustic -print -quit); \
    if [ -z "$p" ]; then echo "rustic binary not found in ${file}"; exit 1; fi; \
    mv "$p" /usr/local/bin/rustic; \
    chmod +x /usr/local/bin/rustic; \
    ls -l /usr/local/bin/rustic

# Stage 3 (Final)
FROM gcr.io/distroless/static:latest
COPY --from=builder /etc/os-release /etc/os-release
COPY --from=builder /etc/mime.types /etc/mime.types
COPY --from=builder /app/elytra /usr/bin/
COPY --from=rust-builder /usr/local/bin/rustic /usr/local/bin/rustic

ENTRYPOINT ["/usr/bin/elytra"]
CMD ["--config", "/etc/elytra/config.yml"]

EXPOSE 8080 2022