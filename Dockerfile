# Stage 1 (Build)
FROM golang:1.24-alpine AS builder

ARG VERSION
RUN apk add --update --no-cache git make mailcap curl bash
WORKDIR /app/
COPY go.mod go.sum /app/
RUN go mod download
COPY . /app/
RUN make download-rustic
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X github.com/pyrohost/elytra/src/system.Version=$VERSION" \
    -v \
    -trimpath \
    -o elytra \
    ./src/cmd/elytra
RUN echo "ID=\"distroless\"" > /etc/os-release

# Stage 2 (Deps)
FROM rust:latest AS rust-builder
RUN curl -L --proto '=https' --tlsv1.2 -sSf https://raw.githubusercontent.com/cargo-bins/cargo-binstall/main/install-from-binstall-release.sh | bash
RUN cargo binstall rustic-rs --no-confirm

# Stage 3 (Final)
FROM gcr.io/distroless/static:latest
COPY --from=builder /etc/os-release /etc/os-release
COPY --from=builder /etc/mime.types /etc/mime.types
COPY --from=builder /app/elytra /usr/bin/
COPY --from=rust-builder /usr/local/cargo/bin/rustic /usr/local/bin/rustic

ENTRYPOINT ["/usr/bin/elytra"]
CMD ["--config", "/etc/elytra/config.yml"]

EXPOSE 8080 2022
