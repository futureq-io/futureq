# FutureQ

**A high-performance, distributed delayed-message queue broker written in Go.**

FutureQ lets producers publish messages with a relative delay and guarantees reliable dispatch to consumers when the delay expires. It combines durable embedded storage, Raft-based replication, and bidirectional gRPC streaming into a single, easy-to-operate binary.

[:octicons-arrow-right-24: Get started](quickstart.md){ .md-button .md-button--primary }
[:octicons-mark-github-24: GitHub repository](https://github.com/futureq-io/futureq){ .md-button }
[:octicons-file-code-24: Protocol definition](https://github.com/futureq-io/protocol){ .md-button }

## Features

- **⏱ Delayed messaging** — enqueue a message with a `delay_ms`; it becomes visible to consumers only after the delay expires. Optional per-message `ttl_ms` discards messages nobody consumed.
- **💾 Durable storage** — disk-backed by [Pebble](https://github.com/cockroachdb/pebble) (CockroachDB's LSM store) with a time-optimized 24-byte key schema, a pure in-memory mode, and an alternative bbolt engine.
- **🔀 High availability** — multi-node replication via [Dragonboat](https://github.com/lni/dragonboat) (multi-group Raft): an event shard for messages plus a metadata group for cluster topology.
- **➕ Dynamic membership** — nodes join and leave a running cluster over gRPC (`JoinCluster` / `LeaveCluster`) with catch-up before voting. No static bootstrap list needed after the first node.
- **👥 Consumer groups & topics** — topic-based routing with fan-out across groups and round-robin dispatch within a group. "Universal" consumers (no group) each receive every message.
- **✅ At-least-once delivery** — in-flight tracking with automatic re-dispatch of unacknowledged messages; batched deletes (a single Raft proposal per batch) amortize LSM tombstone costs.
- **⚙️ Tunable durability** — publish acknowledgements at three levels: *quorum commit*, *leader-local persistence* (fast path), or *fire-and-forget* — with a broker-side minimum enforced by policy.
- **📈 Observability** — Prometheus metrics endpoint and structured logging (zap) out of the box.

## Technology stack

| Concern   | Choice                                          |
| --------- | ----------------------------------------------- |
| Language  | Go 1.26                                         |
| Storage   | Pebble (LSM tree) · bbolt (alternative) · in-memory |
| Consensus | Dragonboat v4 (multi-group Raft)                |
| Transport | gRPC (bidirectional streaming)                  |
| Metrics   | Prometheus                                      |
| Protocol  | [`futureq-io/protocol`](https://github.com/futureq-io/protocol) (Protobuf, v0.2.0) |

## How it works

```text
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

- **Writes** go through Raft as one log entry per batch (or straight to Pebble in standalone mode) and are stored under time-bucketed keys for efficient expiry scans.
- **Reads** are push-based: the dispatcher scans for due messages and routes them to connected consumers through a hub using a round-robin strategy per consumer group.
- **Acks** are batched by a deleter and committed as a single Raft proposal, keeping write amplification low.

Delivery is **at-least-once** — consumers should be idempotent.
