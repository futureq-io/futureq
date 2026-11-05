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
| 8..16   | `bucket`    | Time bucket = `fireAtMs / timeBucket`, where `fireAtMs = enqueuedAtMs + delayMs`. With `timeBucket = 0`, raw milliseconds are used (max precision). |
| 16..24  | `eventID`   | Monotonic counter assigned by the event repository (durable across restarts) |

The sort order is therefore *topic → time bucket → ID*, which makes "give me everything due for topic X before now" a single range scan between two computed bounds:

| Helper             | Bound |
| ------------------ | ----- |
| `TopicLowerBound`  | `[topicHash]` — inclusive |
| `TopicUpperBound`  | `[topicHash + 1]` — exclusive |
| `DueUpperBound`    | `[topicHash][maxBucket + 1]` — exclusive; future buckets are never scanned |

The stored value is the protobuf `StoredMessage` (topic, payload, enqueue time, delay, TTL, indexes). The 24-byte key doubles as the opaque `delivery_tag` consumers receive and echo back on ACK.

Delivery preparation and ACK commands, preparation results, and persisted delivery receipts use Protobuf exclusively. Receipts have an `FQR` version-1 prefix, and batch preparation results have an `FQP` version-1 prefix. JSON delivery formats are no longer supported; development data containing them must be reset, including databases, Raft logs, and snapshots. The removed delivery command numbers (2, 3, and 4) remain reserved. Consumer membership and consumer snapshot metadata still use JSON.

## Engines

### Pebble (default)

LSM-tree engine (`storage.pebble.*`). Scans and iterators run on internal snapshots, so reads never block writes. Options: disk or in-memory mode (`vfs.NewMem()` — same code path, nothing persists, WAL auto-disabled), WAL toggle (auto-off in clustered mode — the Raft log is the WAL), block cache (default 16MiB) and memtable size (default 64MiB).

Bounded `NewIter` calls remember verified empty prefixes in a small in-memory cache. Later snapshots can seek past those prefixes instead of repeatedly walking point tombstones in `First()`. Writes invalidate overlapping proofs before and after commit; inserts behind a discovery cursor remain visible on the next cyclic scan. Pending writes and old snapshots cannot certify a raced gap. The cache retains at most 64 ranges and 64 probes with bounds up to 256 bytes, and starts empty after restart. It changes neither the on-disk format, delivery polling/retry intervals, nor payload cache-validity tokens. The first scan of an uncached range still pays the underlying traversal cost; scans over live receipts still examine them.

### bbolt

Alternative single-file engine (`storage.engine: bolt`). One flat B+-tree bucket preserves the ordered keyspace. Reads use read-only transactions; scans run inside a single `db.View` with cursor seek/next. Batches are buffered and applied in one `db.Update`, which always fsyncs on commit — the sync-mode flag is ignored and `Flush` is a no-op.

### Memory

Not a separate engine: it's Pebble in `mode: memory`. Ephemeral workloads only.

## Event repository

`internal/repository/events.go` sits between handlers/state machine and storage:

- Assigns **monotonic event IDs** — leader-only in Raft mode; the last ID is persisted under `metadata/event-repo/last-id` (big-endian uint64) so restarts never reuse IDs. Replicas advance past applied IDs via `ObserveID` instead of assigning.
- Constructs the 24-byte keys and writes optional secondary indexes (proto-marshaled index payloads pointing back to the key).
