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

### Replica consumption and acknowledgements

Consumers can connect to any event replica. Named groups share a deterministic
assignment based on the event ID modulo the number of consumers, ordered by node
ID and then registration order. Consumers with an empty group ID receive
independent copies; they do not compete with each other.

Before a message's first delivery, the event shard records its recipients: one
interest per connected named group and one per universal subscription. An ACK
completes only that interest. The payload and its receipt state are deleted
together after all required interests finish. Recipient ACKs are batched through
Raft, and completed groups are skipped on later scans. New subscriptions do not
join an already prepared message's recipient list. An incomplete named-group
interest survives disconnects and can resume when the group reconnects.
Universal subscriptions are ephemeral: a later scan releases their interest
after they unregister. TTL and explicit message deletion override these interests
and remove the message across the cluster.

Consumer membership changes pause delivery until replicas drain or time out
their old deliveries. A replica must pass fresh quorum reads for both metadata
and event data to serve messages; its delivery permit lasts one second. If a
replica cannot acknowledge a rebalance, the available replicas can fence it after
`delivery.inFlightTimeout` plus that one-second permit, with scheduling and Raft
latency added. Fencing removes the unavailable replica's subscriptions from the
assignment. Returning replicas must catch up and register live subscriptions
before serving again. Consumer timeouts and a crash before ACK persistence can
cause duplicate deliveries, as permitted by at-least-once delivery.

Fan-out retention means an incomplete named group can retain a payload until it
ACKs, reconnects and ACKs, or the message expires/is explicitly deleted. Each newly
prepared message adds an event-Raft write; throughput with this bookkeeping has
not been benchmarked. Upgrading the command format requires all cluster members
to run this version before replica consumption is enabled.

## Quick Start

### Prerequisites

- Go 1.26+
- (Optional) Docker

### Build & run a standalone node

```bash
git clone https://github.com/futureq-io/futureq.git
cd futureq
go build -o futureq ./cmd/futureq

cp config.example.yaml config.yaml   # adjust as needed
./futureq start -c config.yaml
```

A standalone node (`cluster.enabled: false`) writes directly to Pebble — perfect for local development.

### Run a 3-node cluster

On the first node, enable clustering and list its bootstrap member. Use addresses that other nodes and clients can reach:

```yaml
api:
  grpc:
    advertise: "10.0.0.1:8443"
cluster:
  enabled: true
  nodeId: 1
  shardId: 1
  raft:
    advertise: "10.0.0.1:50005"
    initialMembers:
      1: "10.0.0.1:50005"
```

For each additional node, give it a unique `cluster.nodeId`, its own `api.grpc.advertise` and `cluster.raft.advertise`, and a seed address. For example, node 2 uses:

```yaml
api:
  grpc:
    advertise: "10.0.0.2:8443"
cluster:
  enabled: true
  nodeId: 2
  raft:
    advertise: "10.0.0.2:50005"
  joinSeeds:
    - "10.0.0.1:8443"
```

The `--join` flag overrides `cluster.joinSeeds` for one start:

```bash
./futureq start -c node2.yaml --join 10.0.0.1:8443
./futureq start -c node3.yaml --join 10.0.0.1:8443
```

On first start the node contacts each seed until one accepts its `JoinCluster` request; membership is registered on both the event shard and the metadata group. Restarts detect local Raft data and skip the join flow automatically. `cluster.raft.initialMembers` and `cluster.joinSeeds` cannot both be set in a config file.

### Docker

```bash
docker build -t futureq .
docker run -p 8443:8443 -p 9090:9090 -p 50005:50005 \
  -v $(pwd)/config.yaml:/app/config.yaml \
  futureq start -c /app/config.yaml
```

## Configuration

Every value is documented in [`config.example.yaml`](config.example.yaml), which mirrors the built-in defaults. Key sections:

| Section | Highlights |
| ------- | ---------- |
| `api.grpc` | Listen and advertised addresses, concurrent streams, message sizes, keepalive timeout |
| `cluster` | Node and shard IDs, join seeds, Raft addresses and snapshot tuning |
| `storage` | Pebble disk or memory mode, or a Bolt database file |
| `publish` | Minimum acknowledgement level and Raft proposal timeout |
| `delivery` | Time bucket, dispatch, in-flight, delete, and TTL sweep intervals |
| `observability` | Log level and Prometheus listen address |

Every value can be overridden with environment variables using the `FUTUREQ_` prefix, replacing dots with underscores:

```bash
export FUTUREQ_STORAGE_PEBBLE_DATADIR="/var/lib/futureq/data"
export FUTUREQ_OBSERVABILITY_LOGGING_LEVEL="debug"
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

Metrics are exposed at `observability.metrics.listen` (default `0.0.0.0:9090`) in Prometheus format.

`delivery.consumerQueueSize` sets the sender channel capacity per consumer (default 1024, range 1..65536). Override it with `FUTUREQ_DELIVERY_CONSUMERQUEUESIZE`, for example `2048`. In-flight attempt tracking is bounded by the larger of 1024 and that consumer's queue capacity; larger queues therefore increase per-connection memory capacity.

Delivery lateness is measured against each message's enqueue timestamp plus its requested delay:

- `futureq_delivery_sender_enqueue_lateness_ms`: timestamp immediately before a successful handoff to the sender queue minus the due time. Includes retries; excludes rejected enqueues and time spent waiting in the sender queue.
- `futureq_delivery_send_lateness_ms`: timestamp immediately before `stream.Send()` minus the due time. Includes retries and send attempts that later fail; excludes time blocked in that attempt's `Send()` call.

Both are histograms in milliseconds, labeled by topic. A queued attempt can expire before sending, so their sample populations can differ; subtracting their percentiles does not measure queue wait.

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
