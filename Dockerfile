# Stage 1 (Build)
FROM golang:1.24-alpine AS builder

ARG VERSION
ARG RUSTIC_VERSION=0.10.0
ARG TARGETARCH
RUN apk add --update --no-cache git make mailcap curl bash
WORKDIR /app/
COPY go.mod go.sum /app/
RUN go mod download
COPY . /app/

RUN case "$TARGETARCH" in \
	amd64) \
	ASSET="rustic-v${RUSTIC_VERSION}-x86_64-unknown-linux-musl.tar.gz" \
	;; \
	arm64) \
	ASSET="rustic-v${RUSTIC_VERSION}-aarch64-unknown-linux-musl.tar.gz" \
	;; \
	*) \
	echo "Unsupported architecture: $TARGETARCH"; exit 1 \
	;; \
	esac && \
	mkdir -p /tmp/rustic && \
	curl -L https://github.com/rustic-rs/rustic/releases/download/v${RUSTIC_VERSION}/${ASSET} | tar -zx -C /tmp/rustic


RUN CGO_ENABLED=0 go build \
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
COPY --from=builder /tmp/rustic/rustic /usr/bin/

ENTRYPOINT ["/usr/bin/elytra"]
CMD ["--config", "/etc/elytra/config.yml"]

EXPOSE 8080 2022
