# Storage & key schema

## The storage contract

All engines implement the `storage.DB` interface (`internal/storage/contract.go`): `Get`, atomic `Batch` (Set/Delete + commit with sync mode), and ordered iteration via `NewIter` / `Scan(opts, yield)`. The contract requires:

- **Lexicographic byte ordering** of the whole keyspace — the dispatch and TTL logic depends on range scans.
- `ErrNotFound` sentinel error.
- Lower bound inclusive, upper bound exclusive.
- Yielded key/value slices are valid only inside the yield call — callers copy what they keep.

Because of this contract, the dispatcher and janitor are completely engine-agnostic.

## The 25-byte event key

Every message is stored under a 25-byte, big-endian, lexicographically sortable key (`pkg/utils/keys.go`):

| Bytes   | Field       | Meaning |
| ------- | ----------- | ------- |
| 0–7     | `topicHash` | xxhash64 of the topic name |
| 8–16    | `bucket`    | A 17-bit virtual shard slot followed by the 55-bit time bucket. The time bucket is `(enqueuedAtMs + delayMs) / timeBucket`. With `timeBucket = 0`, raw milliseconds are used. |
| 17–24   | `eventID`   | Monotonic ID finalized in Raft log order, or reserved by the standalone event repository |

Unsharded keys sort by topic, time bucket, and ID. Sharded keys sort by topic,
shard slot, time bucket, and ID. Slot zero selects the unsharded layout. A
sharded slot stores the shard count plus the zero-based shard ordinal. The
largest layout uses 65,536 shards. Time buckets above the 55-bit maximum use
the final unsharded bucket, and delivery still checks the exact scheduled time
stored in the message.

The stored value is the protobuf `StoredMessage` (topic, payload, enqueue time, delay, TTL, indexes). The 25-byte key doubles as the opaque `delivery_tag` consumers receive and echo back on ACK.

Reset development databases, Raft logs, and snapshots created with the previous
24-byte key format before starting this version.

Delivery preparation and ACK commands, preparation results, and persisted delivery receipts use Protobuf exclusively. Receipts have an `FQR` version-1 prefix, and batch preparation results have an `FQP` version-1 prefix. JSON delivery formats are no longer supported; development data containing them must be reset, including databases, Raft logs, and snapshots. The removed delivery command numbers (2, 3, and 4) remain reserved. Consumer membership and consumer snapshot metadata still use JSON.

## Virtual delivery shards

The event leader chooses a uniform random shard for each published message and
includes the packed bucket in its Raft command. Every replica stores the same
key and payload. Each named group divides each layout into contiguous runs of
shards, ordered by replica ID. `delivery.shardDistribution` defaults to
`consumerCount`, which weights replicas by the group's worker counts.
`replicaCount` gives equal weight to each replica with consumers in the group.
The owning replica selects one of its committed local workers using the Hub's
dispatch strategy. Discovery and preload seek past shards outside the union of
the local groups' ranges. Local universal subscribers need every shard. For
unsharded keys and layouts with fewer shards than serving replicas, the group
uses event ID modulo the worker count or the serving replica count, according
to the selected policy.

To change the policy, update the config on all cluster nodes and restart them.
The metadata leader commits its configured policy, then the existing rebalance
fences in-flight delivery before activating the new ownership for queued and
new messages. During a rollout, the elected metadata leader's config determines
the desired policy.

Change `storage.virtualShards` on the nodes and restart them to change the
layout used by the event leader for new publishes. During a rollout, each
message's key identifies its own layout. Existing messages drain at their
original keys, including when the count is reduced to one. Worker or
replica membership changes recalculate ownership for every layout under the
existing rebalance fence. A size change requires no data copy or delivery-tag
rewrite. The option applies to clustered publishes.

## Engines

### Pebble (default)

LSM-tree engine (`storage.pebble.*`). Scans and iterators run on internal snapshots, so reads never block writes. Disk mode supports a WAL toggle for clustered nodes, a block cache (default 16MiB), and a memtable size setting (default 64MiB). With the disk WAL enabled, `walMinSyncInterval` defaults to `500us` and sets the minimum time between syncs. In-memory mode discards data on exit.

Bounded `NewIter` calls remember verified empty prefixes in a small in-memory cache. Later snapshots can seek past those prefixes instead of repeatedly walking point tombstones in `First()`. Writes invalidate overlapping proofs before and after commit; inserts behind a discovery cursor remain visible on the next cyclic scan. Pending writes and old snapshots cannot certify a raced gap. The cache retains at most 64 ranges and 64 probes with bounds up to 256 bytes, and starts empty after restart. It changes neither the on-disk format, delivery polling/retry intervals, nor immutable payload-cache ownership. The first scan of an uncached range still pays the underlying traversal cost; scans over live receipts still examine them.

### bbolt

Alternative single-file engine (`storage.engine: bolt`). One flat B+-tree bucket preserves the ordered keyspace. Reads use read-only transactions; scans run inside a single `db.View` with cursor seek/next. Batches are buffered and applied in one `db.Update`, which always fsyncs on commit — the sync-mode flag is ignored and `Flush` is a no-op.

### Memory

Not a separate engine: it's Pebble in `mode: memory`. Ephemeral workloads only.

## Event repository

`internal/repository/events.go` sits between handlers/state machine and storage:

- Reserves **local proposed event IDs** for the publisher. Raft ordered apply retains a proposal only when it exceeds the applied high-water mark; a stale or duplicate reservation receives the next ID. The watermark includes prior items and entries in the same storage batch, is persisted atomically with messages under `metadata/event-repo/last-id`, and survives deletion, restart, replay, and snapshots. Local reservations never decide final Raft key uniqueness. `ObserveID` advances each replica's local allocator after commit.
- Constructs the 25-byte keys and writes optional secondary indexes (proto-marshaled index payloads pointing back to the key).
