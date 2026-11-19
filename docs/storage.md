# Storage & key schema

## The storage contract

All engines implement the `storage.DB` interface (`internal/storage/contract.go`): `Get`, atomic `Batch` (Set/Delete + commit with sync mode), and ordered iteration via `NewIter` / `Scan(opts, yield)`. The contract requires:

- **Lexicographic byte ordering** of the whole keyspace — the dispatch and TTL logic depends on range scans.
- `ErrNotFound` sentinel error.
- Lower bound inclusive, upper bound exclusive.
- Yielded key/value slices are valid only inside the yield call — callers copy what they keep.

Because of this contract, the dispatcher and janitor are completely engine-agnostic.

## The 24-byte event key

Every message is stored under a 24-byte, big-endian, lexicographically sortable key (`pkg/utils/keys.go`):

| Bytes   | Field       | Meaning |
| ------- | ----------- | ------- |
| 0..8    | `topicHash` | xxhash64 of the topic name |
| 8..16   | `bucket`    | The time bucket, with a virtual shard slot ahead of it for sharded publishes. The time bucket is `(enqueuedAtMs + delayMs) / timeBucket`. With `timeBucket = 0`, raw milliseconds are used. |
| 16..24  | `eventID`   | Monotonic ID finalized in Raft log order, or reserved by the standalone event repository |

Unsharded keys sort by topic, time bucket, and ID. Sharded keys sort by topic,
shard slot, time bucket, and ID. The high bit of the bucket field marks a
sharded key. The next eight bits contain the count plus the zero-based shard
ordinal. The remaining 55 bits contain the time bucket. Larger time buckets
use the unsharded layout.

The stored value is the protobuf `StoredMessage` (topic, payload, enqueue time, delay, TTL, indexes). The 24-byte key doubles as the opaque `delivery_tag` consumers receive and echo back on ACK.

Delivery preparation and ACK commands, preparation results, and persisted delivery receipts use Protobuf exclusively. Receipts have an `FQR` version-1 prefix, and batch preparation results have an `FQP` version-1 prefix. JSON delivery formats are no longer supported; development data containing them must be reset, including databases, Raft logs, and snapshots. The removed delivery command numbers (2, 3, and 4) remain reserved. Consumer membership and consumer snapshot metadata still use JSON.

## Virtual delivery shards

The event leader chooses a uniform random shard for each published message and
includes the packed bucket in its Raft command. Every replica stores the same
key and payload. Each named group divides each layout into contiguous runs of
shards, ordered by serving replica ID and weighted by the group's worker counts.
The owning replica selects one of its committed local workers using the Hub's
dispatch strategy. Discovery and preload seek past shards outside the union of
the local groups' ranges. Local universal subscribers need every shard. If a
group has more serving replicas than a layout has shards, it uses per-worker
modulo ownership for that layout so each worker can still receive messages.

Change `storage.virtualShards` on the nodes and restart them to change the
layout used by the event leader for new publishes. During a rollout, each
message's key identifies its own layout. Existing messages drain at their
original keys, including when the count is reduced or set to zero. Worker or
replica membership changes recalculate ownership for every layout under the
existing rebalance fence. A size change requires no data copy or delivery-tag
rewrite. The option applies to clustered publishes.

## Engines

### Pebble (default)

LSM-tree engine (`storage.pebble.*`). Scans and iterators run on internal snapshots, so reads never block writes. Options: disk or in-memory mode (`vfs.NewMem()` — same code path, nothing persists, WAL auto-disabled), WAL toggle (auto-off in clustered mode — the Raft log is the WAL), block cache (default 16MiB) and memtable size (default 64MiB).

Bounded `NewIter` calls remember verified empty prefixes in a small in-memory cache. Later snapshots can seek past those prefixes instead of repeatedly walking point tombstones in `First()`. Writes invalidate overlapping proofs before and after commit; inserts behind a discovery cursor remain visible on the next cyclic scan. Pending writes and old snapshots cannot certify a raced gap. The cache retains at most 64 ranges and 64 probes with bounds up to 256 bytes, and starts empty after restart. It changes neither the on-disk format, delivery polling/retry intervals, nor immutable payload-cache ownership. The first scan of an uncached range still pays the underlying traversal cost; scans over live receipts still examine them.

### bbolt

Alternative single-file engine (`storage.engine: bolt`). One flat B+-tree bucket preserves the ordered keyspace. Reads use read-only transactions; scans run inside a single `db.View` with cursor seek/next. Batches are buffered and applied in one `db.Update`, which always fsyncs on commit — the sync-mode flag is ignored and `Flush` is a no-op.

### Memory

Not a separate engine: it's Pebble in `mode: memory`. Ephemeral workloads only.

## Event repository

`internal/repository/events.go` sits between handlers/state machine and storage:

- Reserves **local proposed event IDs** for the publisher. Raft ordered apply retains a proposal only when it exceeds the applied high-water mark; a stale or duplicate reservation receives the next ID. The watermark includes prior items and entries in the same storage batch, is persisted atomically with messages under `metadata/event-repo/last-id`, and survives deletion, restart, replay, and snapshots. Local reservations never decide final Raft key uniqueness. `ObserveID` advances each replica's local allocator after commit.
- Constructs the 24-byte keys and writes optional secondary indexes (proto-marshaled index payloads pointing back to the key).
