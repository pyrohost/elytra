# Stage 1 (Build)
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

ARG TARGETPLATFORM
ARG BUILDPLATFORM
ARG TARGETOS
ARG TARGETARCH
ARG VERSION

RUN apk add --update --no-cache git make mailcap

WORKDIR /app/
COPY go.mod go.sum /app/
COPY . /app/

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -ldflags="-s -w -X github.com/pyrohost/elytra/src/system.Version=$VERSION" \
    -v \
    -trimpath \
    -o elytra \
    ./src/cmd/elytra

RUN echo "ID=\"distroless\"" > /etc/os-release

# Stage 2 (Final)
FROM gcr.io/distroless/static:latest

COPY --from=builder /etc/os-release /etc/os-release
COPY --from=builder /etc/mime.types /etc/mime.types
COPY --from=builder /app/elytra /usr/bin/
COPY --from=ghcr.io/rustic-rs/rustic:v0.10.0 /rustic /usr/local/bin/rustic

ENTRYPOINT ["/usr/bin/elytra"]
CMD ["--config", "/etc/elytra/config.yml"]

EXPOSE 8080 2022