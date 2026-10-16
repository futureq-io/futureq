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

- Queueing a message reserves its topic, group, and storage key across every member of that group. Universal consumers each retain their own independent in-flight tracking. Repeated scans cannot send a reserved message to another member of the same group.
- Buffered attempts expire after `delivery.inFlightTimeout` (default 5s) if sending never starts. When the gRPC sender begins a valid attempt, it starts a fresh timeout for sending and acknowledgement, so queue waiting does not consume the ACK window.
- An in-flight delivery blocks re-dispatch to its group until the timeout elapses — then the reservation is released and the message can be re-dispatched on the next pass, possibly to a different member.
- Removal paths:
    - Consumer ACK persistence succeeds (only that recipient's tracking is released; failed batches retain tracking while they retry, subject to the normal timeout and disconnect behavior).
    - Consumer NACK (the gRPC handler releases the entry immediately, making the message re-dispatchable).
    - Consumer disconnect (the hub drops that consumer's in-flight keys instantly).
    - Batched delete applied through Raft (replicas clear their in-flight maps too).

## Deleter

- ACKs feed recipient completions into the deleter via `TryMarkAcknowledged`; explicit deletions and TTL expirations enqueue keys via `MarkDeleted`.
- Every `delivery.deleteBatchInterval` (default 500ms) it flushes the accumulated batch (up to 1024 keys per flush): one Raft `DeleteBatchCmd` proposal in clustered mode — replicated so a new leader can't re-dispatch ACKed messages after failover — or a single storage batch in standalone mode.
- Failed batches are re-queued and retried on the next flush (`delete_failures_total` tracks failures).
- After a successful ACK flush, the hub releases tracking for the committed recipients, allowing the rebalance drain to proceed. The shared payload is deleted only after all required recipients complete. Successful explicit deletions clear tracking for the deleted keys.

## TTL janitor

The dispatcher's inline TTL check only covers topics with live consumers. The janitor sweeps the **entire database** every `delivery.ttlsweepInterval` (default 60s), skips non-event keys (metadata, indexes), and routes expired messages to the deleter — so TTL messages with zero consumers still get cleaned up.

## Delivery guarantees

!!! warning "At-least-once"

    A message is re-dispatched if the consumer never ACKs (timeout), NACKs, or disconnects. Consumers must be idempotent. Duplicate delivery is also possible across a leader failover window in rare cases. There is no exactly-once mode and no per-message dedup at the consume path.
