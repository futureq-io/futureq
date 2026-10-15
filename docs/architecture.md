# Architecture

## The big picture

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

## Standalone vs. clustered

One binary, two operating modes, selected by `cluster.enabled`:

|                       | Standalone (`enabled: false`)                    | Clustered (`enabled: true`) |
| --------------------- | ------------------------------------------------ | -------------------------- |
| Writes               | Direct Pebble batch, fsynced per publish batch   | One Raft log entry per publish batch, applied by the on-disk state machine on every replica |
| Deletes              | Direct storage batch                             | `DeleteBatchCmd` proposed through Raft — replicated, so a new leader can't resurrect ACKed messages |
| Dispatch             | Always                                           | Only on the event-shard leader |
| Consume              | Always                                           | Only on the leader (client SDKs learn the leader's address from the metadata group) |
| Membership           | —                                                | Event shard + metadata group (shard 0), joined over gRPC |

## Write path

1. A producer sends a `PublishBatch` over the `PublishStream` bidi stream.
2. The broker validates (no negative delays/TTLs; batch ack level ≥ broker minimum) and wraps each message into a serialized `StoredMessage`.
3. The leader assigns each message a **monotonic event ID** (persisted across restarts), computes its **time bucket** from `enqueue time + delay`, and hashes the topic.
4. Clustered mode: the whole batch becomes a single `StoreBatchCmd` Raft log entry; replicas apply it verbatim so keys converge. Standalone: one Pebble batch, committed with fsync.
5. The message lands in storage under a 24-byte [time-bucketed key](storage.md) (`topicHash · bucket · eventID`) plus optional secondary indexes.

## Read path

Reads are **push-based** — consumers never poll the broker:

1. A consumer opens a `Subscribe` bidi stream with topic and group; the hub registers it.
2. The [dispatcher](delivery.md) periodically scans *only topics with active consumers* for due buckets (a pure range scan that never touches future buckets).
3. Due messages fan out through the hub: every consumer group gets one copy, round-robin within the group; universal consumers each get every message.
4. Delivered messages become *in-flight* until ACKed; after `inFlightTimeout` they are re-dispatched (at-least-once).

## Delete path

1. Consumer ACKs carry the opaque `delivery_tag` (the raw storage key).
2. The [deleter](delivery.md#deleter) accumulates keys and flushes them every `deleteBatchInterval` as one batched delete — a single Raft proposal in clustered mode — amortizing LSM tombstone costs.
3. Failed delete batches are retried on the next flush.

## Components

| Component          | Location                   | Role |
| ------------------ | -------------------------- | ---- |
| gRPC server        | `internal/api/grpc`        | Producer, consumer, and cluster services; plaintext gRPC with keepalive and size limits from config |
| Dispatcher         | `internal/dispatcher`      | Due-bucket scanning, in-flight tracking, leader gating |
| Hub                | `internal/dispatcher`      | Consumer registry; universal fan-out + round-robin per group |
| Deleter            | `internal/dispatcher`      | Batched deletes with pluggable backend (Raft proposal or direct storage) |
| TTL janitor        | `internal/dispatcher`      | Periodic full-DB sweep for expired unconsumed messages |
| Event state machine | `internal/raft/event`     | Dragonboat on-disk state machine applying `StoreBatchCmd`/`DeleteBatchCmd` to Pebble |
| Metadata group     | `pkg/raft/metadata`        | Replicated cluster topology + nodeID→gRPC-address registry (shard 0) |
| Leader persistence | `internal/raft/leaderpersist` | LogDB decorator enabling leader-ack publishes |
| Event repository   | `internal/repository`      | Monotonic ID assignment, key construction, secondary indexes |
| Storage engines    | `internal/storage`         | Pebble and bbolt behind a common ordered-key contract |
| Config             | `internal/config`          | YAML + env loading with full validation |
| Metrics            | `internal/metrics`         | Prometheus HTTP endpoint (started before Raft, for probes) |
| App wiring         | `internal/app`             | Storage init, Raft lifecycle, graceful shutdown |
