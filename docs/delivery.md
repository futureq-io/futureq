# Delivery pipeline

```text
 storage ──range scan──▶ Dispatcher ──▶ Hub ──▶ consumer channels
                         ▲                │
    ack ──────────────────┘──── Deleter ◀──┘   (delete batches → Raft/storage)
                                  ▲
 expired messages ────────────────┘  (TTL janitor full-DB sweep)
```

## Dispatcher

A timer-driven loop (`delivery.dispatchPollInterval`, default 50ms):

- The hub wakes it immediately when a consumer registers (`wakeCh`).
- After any pass that dispatched at least one message, the next pass fires immediately (drain without waiting); otherwise the full interval elapses.
- It iterates **only topics with active consumers** — topics without consumers are never scanned.
- In Raft mode only the event-shard leader dispatches; standalone always dispatches.
- Per topic it runs a snapshot-based range scan `[topicHash, dueUpperBound(now))` — future buckets are never touched.
- For each due key: skip if in-flight (unless timed out); inline TTL check (expired → routed to the deleter); otherwise build a `QueueMessage` whose `delivery_tag` is the 24-byte key and hand it to the hub.

## Hub & consumer groups

Each consumer registers with a topic and a group and gets a UUID and a buffered channel (capacity 1024). Delivery semantics per topic:

| Consumer kind | `group_id`   | Behavior |
| ------------- | ------------ | -------- |
| Grouped       | non-empty    | **Competing**: exactly one consumer in the group receives each message, chosen round-robin; the pointer rotates per group on every dispatch. |
| Universal     | empty        | **Fan-out**: every universal consumer receives every message; they never compete with each other or with groups. |

Different groups each get an independent copy of each message. Delivery to a channel is *non-blocking*: a slow consumer with a full channel is skipped for that message (with a warning) and, if no consumer in the topic accepted it, the message is simply not marked in-flight and is retried on the next pass.

## In-flight tracking & redelivery

- Once a message is actually sent to at least one consumer, the dispatcher records it in an in-flight map keyed by the raw storage key with the dispatch time.
- An in-flight message blocks re-dispatch until `delivery.inFlightTimeout` (default 5s) elapses — then the entry is dropped and the message is re-dispatched on the next pass, possibly to a different consumer in the group.
- Removal paths:
    - Consumer ACK or NACK (the gRPC handler removes the entry either way — a NACK makes the message immediately re-dispatchable).
    - Consumer disconnect (the hub drops that consumer's in-flight keys instantly).
    - Batched delete applied through Raft (replicas clear their in-flight maps too).

## Deleter

- ACKs and TTL expirations feed keys into the deleter via `MarkDeleted`.
- Every `delivery.deleteBatchInterval` (default 500ms) it flushes the accumulated batch (up to 1024 keys per flush): one Raft `DeleteBatchCmd` proposal in clustered mode — replicated so a new leader can't re-dispatch ACKed messages after failover — or a single storage batch in standalone mode.
- Failed batches are re-queued and retried on the next flush (`delete_failures_total` tracks failures).
- After a successful flush, the dispatcher's in-flight entries for those keys are removed.

## TTL janitor

The dispatcher's inline TTL check only covers topics with live consumers. The janitor sweeps the **entire database** every `delivery.ttlsweepInterval` (default 60s), skips non-event keys (metadata, indexes), and routes expired messages to the deleter — so TTL messages with zero consumers still get cleaned up.

## Delivery guarantees

!!! warning "At-least-once"

    A message is re-dispatched if the consumer never ACKs (timeout), NACKs, or disconnects. Consumers must be idempotent. Duplicate delivery is also possible across a leader failover window in rare cases. There is no exactly-once mode and no per-message dedup at the consume path.
