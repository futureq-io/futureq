# gRPC API

FutureQ speaks gRPC; protobuf definitions live in [futureq-io/protocol](https://github.com/futureq-io/protocol). The server registers three services: `futureq.FutureQProducer`, `futureq.FutureQConsumer`, and `futureq.FutureQCluster`.

## Services overview

| RPC              | Service  | Type            | Description |
| ---------------- | -------- | --------------- | ----------- |
| `PublishStream`  | Producer | bidi streaming  | Publish batches of delayed messages; receive per-batch acks |
| `CreateSubscription` | Consumer | unary | Create durable interest, independently of worker connections |
| `Unsubscribe` | Consumer | unary | Remove interest and discard its pending deliveries |
| `Subscribe`      | Consumer | bidi streaming  | Receive messages for a topic/consumer group; ack over the same stream |
| `GetClusterInfo` | Cluster  | unary           | Cluster topology, leader and member metadata |
| `JoinCluster`    | Cluster  | unary           | Add a node to the event shard and metadata group |
| `LeaveCluster`   | Cluster  | unary           | Gracefully remove a node from the cluster |
| `JoinMetadata`   | Cluster  | unary           | Add a non-voting observer to the metadata group (SDKs/brokers) |
| `LeaveMetadata`  | Cluster  | unary           | Remove a metadata observer |

## PublishStream

`/futureq.FutureQProducer/PublishStream` — bidi stream of `PublishBatch` → `PublishBatchAck`.

- Each `PublishBatch` carries `messages` (topic, payload, delay_ms, ttl_ms, optional indexes) and an `ack_level`.
- Exactly one `PublishBatchAck{success, error_message}` is sent per received batch — **batch-level** semantics: the batch fails atomically (no partial write) on negative delays/TTLs, a too-weak ack level, or non-leader contact in Raft mode. On failure the RPC-level gRPC status also carries the error.
- An empty batch returns a default (unsuccessful) ack and performs no write.
- Client half-close (EOF) ends the stream cleanly.
- Ack levels: `QUORUM` (default; returns after Raft quorum commit), `LEADER` (returns after the leader durably persists the entry — see [ack levels](clustering.md#publish-ack-levels)), `NO_ACK` (fire-and-forget). The broker's `publish.minAckLevel` floors what clients may request.
- In Raft mode only the event-shard leader accepts writes; standalone always writes directly to storage with fsync.
- Destinations are frozen at publication and persisted atomically with each
  payload. Every active durable group or universal subscription is included,
  even with zero connected workers. Later subscriptions receive only future
  publications.
- If any topic in a batch has no subscriptions, the whole batch fails with
  `FailedPrecondition`. Call `CreateSubscription` before publishing.

## CreateSubscription and Unsubscribe

Both RPCs take `SubscriptionRequest{topic, group_id, subscriber_id}`. Set exactly
one of `group_id` and `subscriber_id`; invalid identities return `InvalidArgument`.

`CreateSubscription` is idempotent while that identity exists. It returns a
`SubscriptionResponse` including its generation and creates durable interest
without a worker connection. `group_id` identifies competing workers;
`subscriber_id` identifies an independent universal subscriber across reconnects.

`Unsubscribe` is idempotent and deactivates interest. Bounded background cleanup
discards only its pending deliveries. Other subscriptions remain unaffected;
worker sends can overlap until removal is observed. Recreating the name creates
a fresh generation with no old backlog. Neither consumer disconnects nor stream
retries implicitly unsubscribe or recreate interest.

`delivery.subscriptions.disconnectTimeout` controls automatic unsubscribe after
all workers disconnect. Default `0s` means forever. A never-connected interest
starts its timeout at creation. Connected but idle workers do not expire;
crashed connections are detected by their 15-second session leases before the
configured timeout starts. Message TTL still applies.

## Subscribe

`/futureq.FutureQConsumer/Subscribe` — bidi stream of `ConsumerFrame` → `QueueMessage`.

### Handshake

- The **first** frame must be `ConsumerFrame{init: SubscribeInit{topic, group_id, subscriber_id}}`.
- Create the durable subscription first. Missing subscriptions return `NotFound`; invalid or ambiguous identities return `InvalidArgument`.
- The server confirms a bound generation in initial metadata (`subscription-generation`). Clients must inspect terminal status if no binding header is received.
- Consumers may connect to any event replica. Cluster-wide assignments and
  expiring ownership permits fence delivery during rebalances and partitions.

### After registration

- The broker assigns a UUID and registers the consumer with the [hub](delivery.md#hub-consumer-groups) (buffered channel, capacity 1024).
- `group_id` selects competing workers. Otherwise `subscriber_id` selects a durable universal interest; different IDs receive independent copies, and multiple workers using one ID share its backlog.
- Server → client frames are `QueueMessage` with `delivery_tag` — an opaque tag the client must echo back.
- Client → server frames are `AckRequest{success, delivery_tag}`:
    - `success = true` (ACK): the message is queued for batched deletion and becomes non-re-dispatchable.
    - `success = false` (NACK): nothing is deleted; the message becomes re-dispatchable immediately.
- Any ack frame also removes the in-flight entry for that consumer. Non-ack frames after handshake are logged and skipped.
- Client EOF ends the stream cleanly; the consumer is unregistered and its in-flight keys dropped.
- Disconnect removes only the worker session. Both group and universal subscriptions retain pending deliveries; reconnect with the same topic and identity to resume.

## Cluster RPCs

### GetClusterInfo

- Returns `leader_node_id`, `leader_address`, and the member list (`node_id`, `address`, `is_leader`).
- Any node answers. On a standalone node → `NotFound`; before topology is available → `Unavailable`.
- Node addresses prefer the registered gRPC address, falling back to the Raft address.

### JoinCluster

- Request: `node_id`, `raft_address`, `grpc_address`.
- Must be sent to the leader (not forwarded). Runs add-non-voting → catch-up wait (30s) → promote-to-voter on both the event shard and the metadata group.
- Failures are reported in `JoinResponse{success, error_message}` without failing the RPC status.

### LeaveCluster

Removes the node from the event shard and then the metadata group (leader, same response conventions). Afterwards the node's local Raft data can be deleted.

### JoinMetadata / LeaveMetadata

Add/remove a non-voting observer on the metadata group only — for client SDKs or new brokers that want live topology without participating in consensus.

## Security

!!! danger

    The gRPC server is **plaintext — no TLS and no authentication**. All RPCs, including cluster membership mutations, are open on the configured listen address. Run it on a trusted network or in front of a TLS-terminating proxy.

Server tuning from config: `maxConcurrentStreams` (default 10), max message sizes (100KiB), keepalive (min 5s, permit-without-stream, idle 30s, max connection age 2m + 10s grace, ping 10s, timeout from `api.grpc.keepaliveTimeout`).
