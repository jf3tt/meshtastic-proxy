# Meshtastic Proxy — Agent Instructions

## Project Overview

A TCP proxy for Meshtastic LoRa mesh radio nodes, written in Go. It connects to a single Meshtastic node over TCP, accepts multiple client connections (iOS/Android Meshtastic app, Python CLI, etc.), and multiplexes traffic between them. Includes a web dashboard, mDNS service advertisement, and Kubernetes deployment manifests.

## Architecture

```
Meshtastic Node (TCP :4403)
        │
   node.Connection          ← persistent TCP, auto-reconnect, config caching
        │
   proxy.Proxy              ← accept clients, broadcast FromRadio, relay ToRadio
   ├── proxy.Client[0]      ← per-client read/write loops + send channel
   ├── proxy.Client[1]
   └── ...
        │
   Other components:
   ├── discovery.Advertiser  ← mDNS (_meshtastic._tcp), multi-interface support
   ├── web.Server            ← HTTP dashboard + SSE + metrics API
   └── metrics.Metrics       ← counters, ring buffers, pub/sub for SSE
```

### Package Map

| Package | Path | Purpose |
|---|---|---|
| `main` | `cmd/meshtastic-proxy/` | Entry point, wiring, signal handling |
| `config` | `internal/config/` | TOML config loading & validation |
| `node` | `internal/node/` | Persistent TCP connection to Meshtastic node, config cache |
| `proxy` | `internal/proxy/` | Client connection hub, broadcast, cached config replay |
| `protocol` | `internal/protocol/` | Meshtastic binary frame encoding (magic bytes + length + protobuf) |
| `discovery` | `internal/discovery/` | mDNS advertisement via hashicorp/mdns, multi-interface |
| `metrics` | `internal/metrics/` | Runtime stats, message log, traffic time-series, SSE pub/sub |
| `web` | `internal/web/` | HTTP server, dashboard, SSE endpoint |

### Key Dependencies

- `buf.build/gen/go/meshtastic/protobufs` — Meshtastic protobuf definitions (FromRadio, ToRadio, MeshPacket)
- `github.com/hashicorp/mdns` — mDNS server (one `*mdns.Server` per network interface)
- `github.com/BurntSushi/toml` — config parsing
- `google.golang.org/protobuf` — protobuf runtime

## Wire Protocol

Meshtastic TCP uses a 4-byte frame header: `[0x94] [0xC3] [len_hi] [len_lo]` followed by a protobuf payload (max 512 bytes). See `internal/protocol/frame.go`.

- Node → Proxy: `FromRadio` protobuf (config, packets, telemetry, etc.)
- Proxy → Node: `ToRadio` protobuf (packets, want_config_id, etc.)
- Proxy → Client: same `FromRadio` frames (broadcast + cached config replay)
- Client → Proxy: same `ToRadio` frames (forwarded to node)

## Important Design Decisions

### Cached Config Replay

When a new client connects, the proxy replays the cached node configuration (MyInfo, Config, ModuleConfig, Channel, NodeInfo, ConfigCompleteId — up to 172+ frames). This uses `Client.WriteDirect()` which writes directly to the TCP connection **before** starting the read/write loops (`Client.Run()`). This avoids the send channel (buffer size 128) and prevents "slow consumer" disconnects.

**Call order in `proxy.handleNewConnection`:**
```
sendCachedConfig(client)  → WriteDirect (bypasses channel, writes to conn)
client.Run(ctx)           → Start() + Wait() (channel-based write loop for live traffic)
```

Do NOT change this order. Do NOT use `client.Send()` for cached config — the channel will overflow.

### mDNS Multi-Interface

`discovery.Advertiser` creates a separate `*mdns.Server` per configured network interface. The `hashicorp/mdns` library only supports one `*net.Interface` per server. When `interfaces` config is empty, a single server with system-default interface is used.

### hostNetwork in Kubernetes

mDNS multicast cannot traverse Kubernetes overlay networks. The deployment uses `hostNetwork: true` and the namespace has `pod-security.kubernetes.io/enforce: privileged` because the `baseline` Pod Security Standard blocks hostNetwork.

## Development

### Build & Test

```bash
make build          # build binary to ./bin/
make test           # go test -v -race ./...
make lint           # go vet + golangci-lint
make run            # build + run with config.toml
make docker         # docker compose build
make docker-up      # docker compose up -d
```

### Running Tests

Always use the race detector:

```bash
go test -race -v ./...
```

All tests must pass with `-race`. The project has zero tolerance for data races.

### Git Hooks

Pre-commit hook runs `go vet`, `go test -race`, and lint. Install with:

```bash
make install-hooks
```

### Config File

- Development: `config.toml` (git-tracked, real node address)
- Example/reference: `config.example.toml` (safe defaults)
- Kubernetes: `deploy/kubernetes/configmap.yaml`
- Docker: mounted at `/app/config.toml`
- Kubernetes: mounted at `/etc/meshtastic-proxy/config.toml`

## Deployment

### Docker

Image: `ghcr.io/jf3tt/meshtastic-proxy:latest`

```bash
docker compose up -d
```

### Kubernetes

Target environment: Talos OS, Cilium CNI, MetalLB. Worker node `talos-worker-01` with LAN interface `enx00155dc6c81d` (USB-Ethernet, IP `10.10.0.31/24`).

```bash
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -f deploy/kubernetes/configmap.yaml
kubectl apply -f deploy/kubernetes/deployment.yaml
kubectl apply -f deploy/kubernetes/service.yaml
```

Manifests use `imagePullPolicy: Always` and image tag `latest`.

#### Kubernetes Manifest Summary

| File | Resource | Purpose |
|---|---|---|
| `namespace.yaml` | Namespace `meshtastic` | Privileged PSS for hostNetwork |
| `configmap.yaml` | ConfigMap | TOML config with LAN interface |
| `deployment.yaml` | Deployment | Pod with hostNetwork, HTTP probes on web port |
| `service.yaml` | Service (LoadBalancer) | Exposes web dashboard (8090) via MetalLB |

#### Probes

Health probes use `httpGet` on the **web** port (8090), NOT `tcpSocket` on the proxy port (4404). Using tcpSocket on 4404 creates fake Meshtastic client connections that instantly disconnect, spamming the logs.

## Code Style & Conventions

- Go 1.24+, standard library preferred over third-party when possible
- `log/slog` for structured logging (no `log` or `fmt.Printf` for runtime logging)
- `sync.Mutex` / `sync.RWMutex` for shared state, `sync/atomic` for counters
- Non-blocking sends to channels with `select` + `default` (drop or disconnect on full)
- `context.Context` for cancellation propagation throughout all goroutines
- Errors wrapped with `fmt.Errorf("doing X: %w", err)` for context
- Test files colocated with source (`*_test.go` in same package)

## Known Pitfalls

1. **net.Pipe in tests**: `net.Pipe` is synchronous — `Write` blocks until `Read` consumes. Use TCP loopback (`net.Listen` + `net.Dial` on `127.0.0.1:0`) for tests where the write side must not block.

2. **hashicorp/mdns data races**: The `mdns.Query()` client has internal races after sending entries to a channel. Tests that use `mdns.Query` must synchronize with `sync.WaitGroup`, not rely on channel close timing.

3. **Meshtastic node config size**: The config cache can be 170+ frames. Any code path that delivers these frames must not be bounded by a fixed-size channel.

4. **Talos OS interface names**: The worker node uses `enx00155dc6c81d` (USB-Ethernet, MAC-based naming), not `eth0`. Cilium creates many `cilium_*` and `lxc*` interfaces — the mDNS `interfaces` config must explicitly list the LAN interface to avoid advertising on CNI interfaces.
