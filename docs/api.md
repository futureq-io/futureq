# gRPC API

FutureQ speaks gRPC. The server registers `futureq.FutureQTopic`, `futureq.FutureQProducer`, `futureq.FutureQConsumer`, and `futureq.FutureQCluster`. [Protobuf definitions](https://github.com/futureq-io/protocol)

## Services overview

| RPC              | Service  | Type            | Description |
| ---------------- | -------- | --------------- | ----------- |
| `CreateTopic` | Topic | unary | Declare a topic with optional initial subscriptions |
| `GetTopic` | Topic | unary | Look up a declared topic by name |
| `ListTopics` | Topic | unary | List declared topics in name order |
| `PublishStream`  | Producer | bidi streaming  | Publish batches of delayed messages; receive per-batch acks |
| `CreateSubscription` | Consumer | unary | Create durable interest, independently of worker connections |
| `Unsubscribe` | Consumer | unary | Remove interest and discard its pending deliveries |
| `Subscribe`      | Consumer | bidi streaming  | Receive messages for a topic/consumer group; ack over the same stream |
| `GetClusterInfo` | Cluster  | unary           | Cluster topology, leader and member metadata |
| `JoinCluster`    | Cluster  | unary           | Add a node to the event shard and metadata group |
| `LeaveCluster`   | Cluster  | unary           | Gracefully remove a node from the cluster |
| `JoinMetadata`   | Cluster  | unary           | Add a non-voting observer to the metadata group (SDKs/brokers) |
| `LeaveMetadata`  | Cluster  | unary           | Remove a metadata observer |

## Topic provisioning

Call `CreateTopic` before publishing or creating subscriptions. A topic name
must be valid UTF-8, 1–255 bytes long, and free of control characters.

You can declare a topic and up to 256 initial subscriptions in one atomic RPC.
Every initial subscription sets exactly one of `group_id` and `subscriber_id`.
Repeating the request adds any missing subscriptions and keeps the generations
of active subscriptions. This example provisions an email topic with a group
that can queue jobs before its workers start.

```json
{
  "name": "email.send",
  "initial_subscriptions": [
    { "group_id": "mailers" }
  ]
}
```

`GetTopic` returns `NotFound` for an undeclared name. `ListTopics` includes
topics without subscriptions. Use the returned `next_page_token` as the next
request's `page_token` until it is empty. The default page size is 100 and the
maximum is 256.

## PublishStream

`/futureq.FutureQProducer/PublishStream` — bidi stream of `PublishBatch` → `PublishBatchAck`.

- Each `PublishBatch` carries `messages` (topic, payload, delay_ms, ttl_ms, optional indexes and `max_retries`) and an `ack_level`.
- `max_retries` counts redeliveries independently for each durable subscription.
  Omit it for unlimited retries. Set it to `0` for one attempt or `3` for up to
  four attempts. Exhausted deliveries are retained in the topic's dead-letter queue.
- Each accepted batch receives one `PublishBatchAck`. A rejected batch writes no
  messages and closes the stream with a gRPC error.
- An empty batch returns a default (unsuccessful) ack and performs no write.
- Client half-close (EOF) ends the stream cleanly.
- Ack levels: `QUORUM` (default; returns after quorum commit and successful admission during apply), `LEADER` (returns after the leader durably persists the entry with frozen destinations — see [ack levels](clustering.md#publish-ack-levels)), `NO_ACK` (submission only, with preselected destinations). The broker's `publish.minAckLevel` floors what clients may request.
- In Raft mode only the event-shard leader accepts writes; standalone always writes directly to storage with fsync.
- Destinations are frozen at publication and persisted atomically with each
  payload. Every active durable group or universal subscription is included,
  even with zero connected workers. Later subscriptions receive only future
  publications.
- If any topic in a batch is undeclared, the whole batch fails with `NotFound`.
  A declared topic with no durable destinations fails with `FailedPrecondition`.
- Quorum admission uses subscription state at the publish entry in Raft order;
  it needs no separate admission read. Leader and no-ACK modes select destinations
  before proposal using a fresh registry read. An unroutable batch writes no
  messages. Standalone publishes persist payload and destinations together in
  one synchronous batch.

## CreateSubscription and Unsubscribe

Both RPCs take `SubscriptionRequest{topic, group_id, subscriber_id}`. Set exactly
one of `group_id` and `subscriber_id`; invalid identities return `InvalidArgument`.

`CreateSubscription` requires a declared topic and returns `NotFound` if the
topic is missing. It is idempotent while that identity exists and returns a
`SubscriptionResponse` including its generation and creates durable interest
without a worker connection. `group_id` identifies competing workers;
`subscriber_id` identifies an independent universal subscriber across reconnects.

`Unsubscribe` is idempotent and deactivates interest. The topic stays registered
when its last subscription is removed or expires. Bounded background cleanup
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

Set `SubscribeInit.auto_ack = true` to complete this worker's deliveries after
each successful gRPC send. The default is `false` (manual ACK/NACK). Auto-ack is
per connection and must be requested again on reconnect; durable subscription
creation and generation are unchanged. The server rejects auto-ack streams with
`PermissionDenied` unless `delivery.subscriptions.allowAutoAck: true` is set.
An accepting server includes `subscription-auto-ack: true` in initial metadata;
clients must require that confirmation before suppressing ACK/NACK frames.

On auto-ack streams both manual ACK and NACK requests are ignored. A successful
send completes the subscription's delivery before application processing is
known. Failed sends remain pending. A crash before completion is committed can
cause duplicates.

### After registration

- The broker assigns a UUID and registers the consumer with the [hub](delivery.md#hub-consumer-groups) (buffered channel, capacity 1024).
- `group_id` selects competing workers. Otherwise `subscriber_id` selects a durable universal interest; different IDs receive independent copies, and multiple workers using one ID share its backlog.
- Server → client frames are `QueueMessage` with an opaque `delivery_tag` the client must echo unchanged. When `max_retries` is set, `attempt` is one-based and the tag identifies that specific attempt.
- Client → server frames are `AckRequest{success, delivery_tag}`:
    - `success = true` (ACK) completes this subscription's delivery. The shared payload is deleted after all recipients finish.
    - `success = false` (NACK) makes that subscription eligible for redelivery or archives the delivery if it exhausted its retry limit.
- Non-ack frames after handshake are logged and skipped.
- Client EOF ends the stream cleanly; the consumer is unregistered and its in-flight keys dropped.
- Disconnect removes only the worker session. Both group and universal subscriptions retain pending deliveries; reconnect with the same topic and identity to resume.

## Dead-letter queue

`futureq.FutureQDeadLetter` provides three unary RPCs for retained failures.

| RPC | Request and result |
| --- | --- |
| `ListDeadLetters` | Provide a topic and optional page size and token. Returns summaries ordered by storage key, with a token when more records are available. The default is 100 summaries, the maximum is 256, and the response may end earlier at its byte budget. |
| `GetDeadLetter` | Provide the topic and an opaque ID from the list. Returns the summary and original publish fields, including payload and indexes. A missing record returns `NotFound`. |
| `DeleteDeadLetter` | Provide the topic and ID. Deletion is idempotent. |

A summary identifies the failed subscription and generation, attempt count,
failure reason, and enqueue and dead-letter timestamps. Entries remain until
deleted. Archived payloads shared by several entries are reclaimed after the
last entry is deleted. In Raft mode, reads use a quorum read and deletes are
replicated. The Go SDK exposes the same three operations on `Client`.

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
