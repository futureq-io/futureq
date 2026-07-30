# FutureQ

**A high-performance, distributed delayed-message queue broker written in Go.**

FutureQ lets producers publish messages with a relative delay and guarantees reliable dispatch to consumers when the delay expires. It combines durable embedded storage, Raft-based replication, and bidirectional gRPC streaming into a single, easy-to-operate binary.

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)](go.mod)

---

## Features

- **Delayed messaging** — enqueue a message with a `delay_ms`; it becomes visible to consumers only after the delay expires.
- **Durable storage** — disk-backed by [Pebble](https://github.com/cockroachdb/pebble) (CockroachDB's LSM store) with a time-optimized key schema, or pure in-memory for ephemeral workloads.
- **High availability** — multi-node replication via [Dragonboat](https://github.com/lni/dragonboat) (multi-group Raft), with a metadata Raft group for cluster membership.
- **Dynamic membership** — nodes join and leave a running cluster over gRPC (`JoinCluster` / `LeaveCluster`); no static bootstrap list required after the first node.
- **Consumer groups & topics** — topic-based routing with fan-out across groups and round-robin dispatch within a group.
- **At-least-once delivery** — in-flight tracking with automatic re-dispatch of unacknowledged messages; batched deletes amortize LSM tombstone costs.
- **Message TTL** — a background janitor removes expired messages that were never consumed.
- **Observability** — Prometheus metrics endpoint and structured logging (zap) out of the box.

## Technology Stack

| Concern      | Choice                                        |
| ------------ | --------------------------------------------- |
| Language     | Go 1.26                                       |
| Storage      | Pebble (LSM tree)                             |
| Consensus    | Dragonboat (multi-group Raft)                 |
| Transport    | gRPC (bidirectional streaming)                |
| Metrics      | Prometheus                                    |
| Protocol     | [`futureq-io/protocol`](https://github.com/futureq-io/protocol) (Protobuf) |

## Architecture Overview

```
                ┌──────────────┐   PublishStream   ┌──────────────────────────┐
                │  Producer    │ ─────────────────▶│                          │
                └──────────────┘                   │        FutureQ Node       │
                                                   │                          │
                ┌──────────────┐   Subscribe       │  ┌────────────────────┐  │
                │  Consumer    │ ◀──────────────── │  │ gRPC API (8443)    │  │
                └──────────────┘                   │  └─────────┬──────────┘  │
                                                   │            ▼             │
                ┌──────────────┐   Raft (50005)    │  ┌────────────────────┐  │
                │  Other Nodes │ ◀───────────────▶ │  │ Dispatcher / Hub   │  │
                └──────────────┘                   │  │ Deleter · Janitor  │  │
                                                   │  └─────────┬──────────┘  │
                ┌──────────────┐   Prometheus      │            ▼             │
                │  Metrics     │ ◀── (9090) ──────│  │ Pebble + Raft log    │  │
                └──────────────┘                   │  └────────────────────┘  │
                                                   └──────────────────────────┘
```

- **Writes** go to the Raft leader (or straight to Pebble in standalone mode) and are stored under time-bucketed keys for efficient expiry scans.
- **Reads** are push-based: a dispatcher continuously scans for matured messages and routes them to connected consumers through a hub using a round-robin strategy.
- **Acks** are batched by a deleter and committed as a single Raft proposal, keeping write amplification low.

Delivery is **at-least-once** — consumers should be idempotent.

## Quick Start

### Prerequisites

- Go 1.26+
- (Optional) Docker

### Build & run a standalone node

```bash
git clone https://github.com/futureq-io/futureq.git
cd futureq
go build -o futureq ./internal/main.go

cp config.example.yaml config.yaml   # adjust as needed
./futureq start -c config.yaml
```

A standalone node (no `raft` section, or `raft.enabled: false`) writes directly to Pebble — perfect for local development.

### Run a 3-node cluster

On the first node, enable Raft and list all initial members:

```yaml
raft:
  enabled: true
  nodeId: 1
  clusterId: 1
  listenAddress: "0.0.0.0:50005"
  initialMembers:
    1: "10.0.0.1:50005"
```

Additional nodes join dynamically — no need to edit `initialMembers`:

```bash
./futureq start -c node2.yaml --join 10.0.0.1:8443
./futureq start -c node3.yaml --join 10.0.0.1:8443
```

On first start the node contacts each seed until one accepts its `JoinCluster` request; membership is registered on both the event shard and the metadata group. Restarts detect local Raft data and skip the join flow automatically.

### Docker

```bash
docker build -t futureq .
docker run -p 8443:8443 -p 9090:9090 -p 50005:50005 \
  -v $(pwd)/config.yaml:/app/config.yaml \
  futureq start -c /app/config.yaml
```

## Configuration

Every value is documented in [`config.example.yaml`](config.example.yaml), which mirrors the built-in defaults. Key sections:

| Section          | Highlights                                                        |
| ---------------- | ----------------------------------------------------------------- |
| `server`         | gRPC listen address, connection limits, message size caps         |
| `storage`        | Engine (`pebble`), persistence toggle, time-bucket granularity    |
| `storage.pebble` | WAL toggle, data path, cache/memtable sizing                      |
| `raft`           | Node/cluster IDs, listen address, initial members, snapshot tuning |
| `consumer`       | Dispatch poll interval, batched-delete interval, in-flight timeout, TTL janitor interval |
| `observability`  | Log level, Prometheus listen address                              |

Every value can be overridden with environment variables using the `FUTUREQ_` prefix, replacing dots with underscores:

```bash
export FUTUREQ_STORAGE_PEBBLE_DATAPATH="/var/lib/futureq/data"
export FUTUREQ_OBSERVABILITY_LOGGER_LEVEL="debug"
```

## API

FutureQ speaks gRPC; protobuf definitions live in [`futureq-io/protocol`](https://github.com/futureq-io/protocol).

| RPC                | Type                | Description                                        |
| ------------------ | ------------------- | -------------------------------------------------- |
| `PublishStream`    | bidi streaming      | Publish batches of delayed messages; receive per-batch acks |
| `Subscribe`        | bidi streaming      | Receive messages for a topic/consumer group; ack over the same stream |
| `GetClusterInfo`   | unary               | Cluster topology, leader and member metadata       |
| `JoinCluster`      | unary               | Add a node to the event shard and metadata group   |
| `LeaveCluster`     | unary               | Gracefully remove a node from the cluster          |
| `LeaveMetadata`    | unary               | Remove a node from the metadata group only         |

Metrics are exposed at `observability.metrics.addr` (default `:9090`) in Prometheus format.

## Project Layout

```
internal/
  main.go          # entrypoint
  cmd/             # Cobra CLI (start, leave)
  app/             # wiring: storage, repositories, Raft lifecycle
  api/grpc/        # gRPC server + handlers (producer, consumer, cluster)
  dispatcher/      # scan/dispatch loop, hub, deleter, TTL janitor
  storage/         # Pebble engine, time-bucket key schema
  raft/            # Dragonboat state machine & event commands
  repository/      # event repository abstraction
  config/          # config loading + env overrides
  metrics/         # Prometheus server
pkg/
  raft/metadata/   # metadata-group Raft (cluster membership)
  log/             # zap logger setup
  utils/           # shared helpers
```

## Development

```bash
go build ./...      # build
go test ./...       # run tests
golangci-lint run   # lint
```

Contributions are welcome — please open an issue to discuss substantial changes before sending a PR.

## License

[MIT](LICENSE)
