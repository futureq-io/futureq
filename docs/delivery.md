# Delivery pipeline

```text
 storage ──range scan──▶ Dispatcher ──▶ Hub ──▶ consumer channels
                         ▲                │
    ack ──────────────────┘──── Deleter ◀──┘   (delete batches → Raft/storage)
                                  ▲
 expired messages ────────────────┘  (TTL janitor full-DB sweep)
```

## Dispatcher

A timer-driven loop (`delivery.dispatchPollInterval`, default 5ms) scans topics
with active local consumers. Each topic has at most one bounded preparation job,
and a fixed worker pool limits outstanding work. Worker completions and hub wake
signals also trigger scheduling. Clustered replicas filter candidates using the
current committed ownership view and delivery permission.

The live due-message scan uses a snapshot and an independent cyclic cursor.
Wrapping the cursor revisits older insertions, failed preparations, full consumer
queues, NACKs, and timeout retries. Iterators close before preparation begins.

### Caching upcoming messages

Two bounded discovery workers load owned full-message data into per-topic heaps.
The scheduler selects exact due times independently of scans and quorum work.
`delivery.preloadLookahead` controls future discovery (default disabled; opt-in trial: 100 ms), and
`delivery.preloadMaxMessages` bounds future messages across topics (default 512).
A separate bounded due allowance keeps ordinary delivery working when either
future setting is zero and when the future cache is full.

A timer wakes at eligible cached deadlines. Recipients are captured when due,
using current ownership and epoch. Delivery workers prepare receipts, perform the
post-preparation read barrier, validate payload versions, read current committed
ACK state, recheck exact due time/TTL and enqueue through the existing Hub.
Valid cached data avoids a second full-payload Get/decode in final delivery;
uncertain/invalidated data uses a counted fresh-read fallback. Preparation still
performs existence reads. The original permit and actual-send fences remain.

Due and future cursors are independent cyclic scan positions, never completion
watermarks. Late arrivals, retries, rejected admission and stale results recover
through due scans even without arrival hints. Failed jobs evict their payloads
and retry after bounded backoff. Topic generations reject stale scan/delivery
results after subscription changes. Storage batch invalidation covers deletion,
replay, overwrite and snapshot replacement before old data can validate.

See the [scheduler design](heap-scheduler.md) for the validity proof, lifecycle,
count sizing, resource table and [measured tradeoffs](heap-scheduler-evaluation.md).

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
