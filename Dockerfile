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

RUN \
    ASSET="rustic-v0.10.0-x86_64-unknown-linux-musl.tar.gz"; \
    wget https://github.com/rustic-rs/rustic/releases/download/v0.10.0/${ASSET} && \
    mkdir -p /rustic && \
    tar -xzf ${ASSET} -C /rustic && \
    mkdir /etc_files && \
    touch /etc_files/passwd && \
    touch /etc_files/group

# Stage 2 (Final)
FROM gcr.io/distroless/static:latest

COPY --from=builder /etc/os-release /etc/os-release
COPY --from=builder /etc/mime.types /etc/mime.types
COPY --from=builder /app/elytra /usr/bin/

COPY --from=builder /etc_files/ /etc/
COPY --from=builder /rustic/rustic /usr/bin/

ENTRYPOINT ["/usr/bin/elytra"]
CMD ["--config", "/etc/elytra/config.yml"]

EXPOSE 8080 2022
