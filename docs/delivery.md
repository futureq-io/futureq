# Delivery pipeline

Consumers use manual ACK/NACK by default. An explicitly requested auto-ack stream
requires `delivery.subscriptions.allowAutoAck: true`. After a successful gRPC
send, the sender records that recipient's completion without waiting for the
application's handler. Failed sends remain pending and other recipients retain
their copies. Manual ACKs and NACKs on auto-ack streams are ignored. A crash
before completion commits can cause duplicates.

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

A timer wakes at eligible cached deadlines. Destinations were already frozen at
publication and committed atomically with the payload. Delivery workers read that
receipt without another write or per-batch read barrier, validate payload
versions, read locally applied committed ACK state, recheck exact due time/TTL
and enqueue through the existing Hub. Clustered workers still need a valid
ownership permit, renewed by periodic quorum reads. Lagging replicas can produce
duplicates before observing another replica's ACK, as allowed by at-least-once.
Valid cached data avoids a second full-payload Get/decode in final delivery;
uncertain/invalidated data uses a counted fresh-read fallback. The original
permit and actual-send fences remain. Background subscription maintenance
releases obligations only after explicit unsubscribe or configured expiry. Disconnect alone never releases interest.

Due and future cursors are independent cyclic scan positions, never completion
watermarks. Late arrivals, retries, rejected admission and stale results recover
through due scans even without arrival hints. Failed jobs evict their payloads
and retry after bounded backoff. Topic generations reject stale scan/delivery
results after subscription changes. Storage batch invalidation covers deletion,
replay, overwrite and snapshot replacement before old data can validate.

## Hub & consumer groups

Declare durable subscriptions with `CreateTopic` during provisioning or add them
to an existing topic with `CreateSubscription`. They exist independently of
worker connections. Set exactly one of:

| Identity | Behavior |
| --- | --- |
| `group_id` | Workers sharing the group compete for one delivery per message. |
| `subscriber_id` | Stable universal identity with an independent copy; reconnect with the same ID. Multiple workers on that ID share its one backlog. |

`Subscribe` only attaches a worker to an existing subscription; it cannot create
or recreate interest. The server confirms binding in the stream's initial
metadata. Consumer groups and universal subscribers use the same durable
lifecycle in standalone and Raft mode. Destinations are frozen at publication,
including offline subscriptions, and never expanded by retry or reconnect. A
batch containing a declared topic without subscriptions is rejected atomically
with `FailedPrecondition`. An undeclared topic returns `NotFound`.

`Unsubscribe` immediately deactivates the subscription in the event shard;
bounded background cleanup discards its pending deliveries and preserves other
subscriptions' interests. Buffered/in-flight sends can overlap removal until
workers observe it. A generation-specific recipient prevents old ACKs or a
recreated name from affecting new deliveries.

`delivery.subscriptions.disconnectTimeout` is `0s` by default (forever). A positive
duration auto-unsubscribes after the last worker disconnects. Connected but idle
workers do not expire. Creation without workers starts the disconnect clock.
Worker sessions have a 15-second lease, renewed every 5 seconds in batches per
broker, with at most 256 sessions per batch and four concurrent renewal batches
by default (`delivery.subscriptions.renewalWorkers`, 1..16).
Each batch checks its result independently; failure does not skip other batches.
After an unclean exit, disconnect starts at the last lease deadline;
expiry then waits the configured disconnect timeout. Maintenance runs each
second on the event leader and handles at most 256 subscription records and 256
receipts per pass, so cleanup/expiry can take longer for larger backlogs. Message
TTL remains independent and can remove messages even for forever subscriptions.

Expiry and retired-receipt cleanup run in one background worker, independently
of session renewal. Slow cleanup proposals cannot hold up renewal. Each worker
checks persistence results; expiry and cleanup revalidate the current generation
and recipient state when applied. Shutdown cancels and joins the maintenance
workers before storage closes.

Stream teardown stops local session renewal immediately and queues persistence
to a disconnect pool (up to 256 sessions per command). It defaults to one worker;
`delivery.subscriptions.disconnectWorkers` accepts 1..16. Its shared queue holds at
most 1024 sessions; failed commands retry. A full queue or shutdown falls back
to lease expiry, so the automatic unsubscribe clock can start later.
Raft consumer membership removal defaults to one background worker; configure
`delivery.consumerRemovalWorkers` from 1..16. Its shared queue remains bounded at
1024 entries. Local unregister still happens first under the registry lock.
Removals target unique stream IDs; delayed retries cannot remove a replacement
stream. Periodic node reconciliation repairs overflow or interrupted removals.
Consumer registration and rebalance activation retain their completion checks.

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

## Retry limits and dead letters

For a publish with `max_retries`, the broker reserves a recipient-specific
attempt and durably charges it before calling gRPC Send. The reservation
deadline includes two `publish.proposalTimeout` windows and one
`delivery.inFlightTimeout` window. The full ACK window starts when the
sender is ready to enter transport, after authorization commits and the
sender checks its permit and reservation deadline. If either check fails, it
refunds the unsent attempt. Once the sender enters transport, the attempt
retains its number through stalls, failures, and broker crashes. Each attempt
has a distinct tag, so a delayed ACK or NACK cannot settle a later attempt.
A failed refund stays protected from expiry while a bounded worker pool retries
refunds independently. The broker reserves capacity for authorization and refunds
before creating an attempt, using `delivery.ackQueueCapacity` as the bound
for this work and independently for the ACK queue. Chunked sends prepare and
validate the first frame while the attempt is still protected for a refund.
The broker checks every later frame and ends the stream if the delivery is
no longer eligible.

The broker queues outcomes for asynchronous persistence and releases local
tracking after commit. While a broker owns an attempt, outcomes it accepts
into a queue take precedence over expiry. Retry-limited deliveries have
separate manual and automatic outcome queues and persistence workers.
Each queue reserves capacity before the broker charges an attempt and
counts its outstanding reservations toward `delivery.ackQueueCapacity`.
A manual ACK or NACK uses its reserved slot when received. An automatic
ACK uses its slot after transport succeeds. Snapshot recovery releases slots
for attempts missing from the restored receipts. On graceful shutdown, the
broker stops granting new reservations and honors active reservations within
the shutdown window. Closing a consumer stream releases its unused manual
slots, and its durable attempts keep their expiry deadlines. The broker keeps
proposals and lease renewal active while it drains accepted outcomes and
refunds unsent attempts. Clustered brokers renew
`delivery.retryOwnerLease` every fifth of its duration (every 3 seconds by
default). Leaders wait an additional `delivery.retryOwnerMaxClockSkew`
(default 2 seconds) before recovering another broker's expired attempts.
The margin covers the maximum wall-clock difference between brokers.
An outage that prevents outcomes from committing before recovery can still
result in a retry or a dead letter.

Deadline scans drain expired attempts in bounded batches, including when no
workers are connected. Expiry releases the matching local slot on every replica
after commit. Deletion removes the durable retry deadlines with the
receipt and stops local transport timers after commit. A final NACK or timeout
atomically archives the payload and marks that subscription's delivery
complete. Other subscriptions keep their independent retry budgets. Message
TTL and unsubscribing can discard pending deliveries before they are
dead-lettered. Retiring a subscription stops its transport timers when cleanup
commits, while other recipients keep their ACK windows. See the
[dead-letter queue API](api.md#dead-letter-queue) for retention and retrieval.

## TTL janitor

The dispatcher's inline TTL check only covers topics with live consumers. The janitor sweeps the **entire database** every `delivery.ttlsweepInterval` (default 60s), skips non-event keys (metadata, indexes), and routes expired messages to the deleter — so TTL messages with zero consumers still get cleaned up.

## Delivery guarantees

!!! warning "At-least-once"

    A message is eligible for redelivery after a NACK, timeout, or disconnect while the subscription has retry budget. Consumers must handle duplicates, including across leader failover.
