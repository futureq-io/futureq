# Raft clustering

Replication is built on [Dragonboat v4](https://github.com/lni/dragonboat) (multi-group Raft). Each clustered node participates in **two** Raft groups.

## Two Raft groups

|                     | Event shard                                      | Metadata shard |
| ------------------- | ------------------------------------------------ | -------------- |
| Shard ID            | `cluster.shardId` (default 1)                    | `0` (fixed, exported from `pkg/raft/metadata`) |
| State machine       | `EventStateMachine` — on-disk (IOnDiskStateMachine) over the same Pebble DB | `MetadataStateMachine` — in-memory; rebuilt from the log on restart |
| Holds               | All messages, indexes, and metadata keys         | Per-shard topology (leader, members, addresses) + nodeID→gRPC-address registry |
| Raft settings       | Election RTT 10, heartbeat RTT 1, CheckQuorum; snapshot intervals from config | Same timers; aggressive snapshots (every 5 entries) — state is tiny |

!!! note

    In clustered mode the application **Pebble WAL may be disabled** with `storage.pebble.walEnabled: false`. The durable Raft log supplies recovery; the state machine flushes data and its applied index at synchronization points and replays the missing committed suffix after restart.

## Event-shard commands

Event commands include the storage format in `internal/raft/event/commands.go`
and publish-bound delivery in `internal/raft/event/published_delivery.go`:

| Command             | Payload |
| ------------------- | ------- |
| `StoreBatchCmd (0)` | Per item: `u64 id` (proposed event ID; ordered apply advances stale IDs past the applied watermark so keys converge) · `u64 bucket` · `u64 topicHash` · index entries · `u32 len` + pre-serialized `StoredMessage`. One publish batch = one log entry. |
| `DeleteBatchCmd (1)`| Count + fixed 24-byte message keys — used for ACK-driven deletes and TTL expirations. |
| `StoreDeliveryBatchCmd (8)` | Leader/no-ACK publish: encoded store batch followed by a length-prefixed frozen receipt per message; payload and destinations commit atomically. |
| `SubscriptionCmd (9)` | Protobuf subscription create/remove, worker connect/disconnect, batched session renewal, conditional idle expiry, and bounded retired-recipient cleanup. |
| `PublishBatchCmd (10)` | Quorum publish: encoded store batch without receipts. Apply selects destinations from subscription state at this log position and writes payload plus receipt atomically. |

Quorum publishers use one proposal with no admission quorum read. Apply merges
persisted subscriptions with earlier subscription commands in the same Update,
so every replica chooses the same recipients. A missing or over-capacity destination
set rejects the entire publish through the proposal result, without stopping the
state machine or writing any messages. Leader and no-ACK publishers catch up the
event view and freeze durable destinations before proposal, preserving early ACKs. The
event group stores group and universal subscriptions independently of the metadata
group's live worker assignments. Subscription records, session leases and retired
generation markers are included in snapshots. Unsubscribe and expiry cannot be
undone by a stale worker reconnect or receipt-cleanup command. Publish-bound delivery therefore needs no per-batch
preparation proposal or post-preparation read barrier. Periodic quorum reads
still renew the bounded ownership permit, and receipt reads observe locally
applied committed state. Lag in observing an ACK can produce duplicate delivery.

`Update` applies all entries in a log batch into one Pebble batch (nothing partial on error), persists the max applied event ID and `metadata/raft/applied-index`, and fires the `OnDeleteKeys` hook so every replica clears dispatcher in-flight entries for deleted keys. Snapshots stream the whole DB as length-prefixed records; recovery clears the DB first and validates the applied index.

## Metadata group

A `metadata.Service` registers as Dragonboat's Raft/system event listener. On leader or membership changes (ignoring shard 0 itself) it snapshots the shard membership into an `UpdateTopologyCmd` and proposes it to the metadata group with an ever-increasing epoch. Each node also proposes `RegisterNodeAddrCmd(nodeID, api.grpc.advertise)` at startup, retrying until the metadata group has a leader. The result: every node (and any SDK that joins as an observer) has a replicated view of the cluster topology, including **the leader's client-facing gRPC address** — that's what client SDKs dial.

## Join & leave flows

### Joining (fresh node)

1. The fresh node (no local Raft data) dials each seed's gRPC address and calls `JoinCluster` with its node ID, Raft address, and gRPC address.
2. The seed runs the three-step dance on *both* groups:
    1. `SyncRequestAddNonVoting` — join without affecting quorum;
    2. wait for catch-up (polling membership every 500ms, 30s deadline);
    3. `SyncRequestAddReplica` — promote to voter.
3. The joining node starts its Raft groups in "join" mode; membership was registered via the RPC.

!!! warning

    `JoinCluster` must reach the **leader** (membership ops fail on followers); the RPC is not forwarded. Clients should ask `GetClusterInfo` first or retry across seeds.

### Leaving

- `futureq leave --seed <addr>` (or a direct `LeaveCluster` RPC) removes the node's replica from the event shard and then the metadata group.
- `LeaveMetadata` removes a metadata-group *observer* only.

### Restart

- If local Raft data exists (`logdb-0/MANIFEST-000001`), the node skips the join flow and starts as an existing member.
- On `ErrShardNotBootstrapped` (e.g. stale data from an aborted bootstrap), the app wipes `cluster.raft.dataDir` so the next start retries cleanly instead of crash-looping.

## Leader persistence

`internal/raft/leaderpersist` decorates Dragonboat's LogDB ("tan+leaderpersist"). After every successful `SaveRaftState` it hashes each persisted application command, removing Dragonboat's uncompressed encoding header, and notifies the tracker. This is the mechanism behind the **leader ack level**: a publisher is acknowledged once the leader has durably written the entry to its local Raft log — before quorum commit. Successful apply is also accepted if it wins the notification race. Before reporting a proposal failure, cancellation, or timeout, the handler rechecks local persistence so an already-persisted command is not reported as failed just because both signals were ready. FutureQ uses uncompressed log entries; unknown encodings fall back to apply completion. Correlation is by command hash; batch commands embed proposed event IDs; ordered apply may assign higher final IDs.

## Publish ack levels

| Level | Durability | Mechanism |
| ----- | ---------- | ---------- |
| `ACK_LEVEL_QUORUM` (default) | Returns after majority commit and successful admission during local apply | `SyncPropose`, no admission read; bounded by `publish.proposalTimeout` (default 5s) |
| `ACK_LEVEL_LEADER` | Returns once the leader durably persisted the entry locally; followers replicate asynchronously | Async propose + leaderpersist tracker wait. **Can lose data on leader crash.** |
| `ACK_LEVEL_NO_ACK` | Fire-and-forget; success means "submitted" only | Async propose, no wait |

The broker enforces a configurable floor: `publish.minAckLevel` (`quorum` | `leader` | `noAck`, default `quorum`). A batch requesting a level weaker than the floor is rejected with `InvalidArgument`. An omitted level defaults to QUORUM; unknown enum values are rejected.

All modes establish publish-time delivery obligations and retry unacknowledged
deliveries after acceptance and commit, subject to unsubscribe, configured expiry,
message TTL, and explicit deletion. They do not provide equal publish durability:
an uncommitted leader-ACK entry can be overwritten after leadership changes even
if its disk survives; no-ACK success confirms submission only. Use quorum ACKs
when producer-confirmed at-least-once delivery must survive leader failure.

## Restart & recovery

- Event IDs never regress: the last ID is persisted in the Pebble metadata keyspace and replicas observe applied IDs.
- Committed-but-unapplied log entries are replayed on restart (including when application Pebble WAL is disabled).
- Snapshots restore the full DB (validated against the applied index); the metadata state machine rebuilds from its log.
- Recipient completions and final deletion are Raft-replicated. A lagging
  replica or uncertain ACK completion may cause duplicates; unacknowledged
  named-group obligations and their payloads survive restart and reassignment.
