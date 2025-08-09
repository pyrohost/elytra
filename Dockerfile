# Stage 1 (Build)
FROM golang:1.23-alpine AS builder

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# Install build dependencies
RUN apk add --update --no-cache git make ca-certificates tzdata mailcap curl

# Set working directory
WORKDIR /app

# Download and install Rustic
ARG RUSTIC_VERSION=v0.9.5
RUN case "${TARGETARCH}" in \
        "amd64") RUSTIC_ARCH="x86_64" ;; \
        "arm64") RUSTIC_ARCH="aarch64" ;; \
        *) echo "Unsupported architecture: ${TARGETARCH}" && exit 1 ;; \
    esac && \
    curl -L "https://github.com/rustic-rs/rustic/releases/download/${RUSTIC_VERSION}/rustic-${RUSTIC_VERSION}-${RUSTIC_ARCH}-unknown-linux-musl.tar.gz" \
    -o rustic.tar.gz && \
    tar -xzf rustic.tar.gz && \
    mv rustic /usr/local/bin/rustic && \
    chmod +x /usr/local/bin/rustic && \
    rm rustic.tar.gz

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X github.com/pyrohost/elytra/src/system.Version=${VERSION}" \
    -trimpath \
    -o elytra \
    src/elytra.go

# Create os-release for distroless
RUN echo 'ID="elytra"' > /etc/os-release && \
    echo 'NAME="Elytra Container"' >> /etc/os-release && \
    echo 'VERSION="'${VERSION}'"' >> /etc/os-release

# Stage 2 (Final)
FROM gcr.io/distroless/static:nonroot

# Copy certificates and mime types
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /etc/mime.types /etc/mime.types
COPY --from=builder /etc/os-release /etc/os-release

# Copy the binaries
COPY --from=builder /app/elytra /usr/bin/elytra
COPY --from=builder /usr/local/bin/rustic /usr/bin/rustic

# Use non-root user
USER nonroot:nonroot

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD ["/usr/bin/elytra", "--help"]

# Expose ports
EXPOSE 8080 2022

# Set entrypoint and default command
ENTRYPOINT ["/usr/bin/elytra"]
CMD ["--config", "/etc/pterodactyl/config.yml"]
