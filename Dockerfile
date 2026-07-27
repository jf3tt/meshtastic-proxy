# Build stage
# --platform=$BUILDPLATFORM + TARGETOS/TARGETARCH cross-compile the Go binary
# natively on the build host instead of emulating the target arch via QEMU,
# so multi-arch (linux/amd64, linux/arm64) builds stay fast.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /bin/meshtastic-proxy ./cmd/meshtastic-proxy

# Runtime stage
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata
RUN adduser -D -h /app appuser

WORKDIR /app

COPY --from=builder /bin/meshtastic-proxy /app/meshtastic-proxy
COPY config.example.toml /app/config.example.toml

USER appuser

# 4404 = proxy TCP, 8080 = web dashboard, 5353/udp = mDNS
EXPOSE 4404 8080 5353/udp

ENTRYPOINT ["/app/meshtastic-proxy"]
CMD ["-config", "/app/config.toml"]
