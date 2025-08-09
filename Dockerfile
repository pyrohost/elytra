# Stage 1 (Build)
FROM golang:1.23-alpine AS builder

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# Install build dependencies
RUN apk add --update --no-cache git make ca-certificates tzdata mailcap

# Set working directory
WORKDIR /app

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

# Copy the binary
COPY --from=builder /app/elytra /usr/bin/elytra

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
