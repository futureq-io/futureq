# Configuration

Configuration is a YAML file passed via `futureq start -c config.yaml`. Every value is documented in [`config.example.yaml`](https://github.com/futureq-io/futureq/blob/main/config.example.yaml), which mirrors the built-in defaults. The config must carry `configVersion: 1`.

## Loading & precedence

Values are merged in this order (later wins):

1. **Built-in defaults** — the default config serialized to YAML.
2. **Config file** — merged key-by-key.
3. **Environment variables** — via viper automatic env binding.

The binary uses `viper.UnmarshalExact`, so unknown keys in the file are rejected rather than silently ignored.

## Environment overrides

Every value can be overridden with the `FUTUREQ_` prefix, dots replaced by underscores (dashes too):

```bash
export FUTUREQ_STORAGE_PEBBLE_DATADIR="/var/lib/futureq/data"
export FUTUREQ_OBSERVABILITY_LOGGING_LEVEL="debug"
export FUTUREQ_PUBLISH_MINACKLEVEL="leader"
export FUTUREQ_API_GRPC_LISTEN="0.0.0.0:8443"
```

Maps cannot come from a single env string, so `FUTUREQ_CLUSTER_RAFT_INITIALMEMBERS` is special-cased: its value is parsed as YAML, e.g.:

```bash
export FUTUREQ_CLUSTER_RAFT_INITIALMEMBERS='{1: "10.0.0.1:50005"}'
```

## api.grpc

| Key                            | Default          | Description |
| ------------------------------ | ---------------- | ----------- |
| `api.grpc.listen`              | `0.0.0.0:8443`   | Address the gRPC server binds to. |
| `api.grpc.advertise`           | `""`             | Client-facing address registered in the cluster. Must be a non-wildcard address (no `0.0.0.0`/`::`) when `cluster.enabled`. |
| `api.grpc.maxConcurrentStreams` | `10`             | gRPC `MaxConcurrentStreams` per connection. |
| `api.grpc.maxReceiveMessageSize`| `100KiB`         | Max inbound message size. Sizes accept B/KiB/MiB/GiB units. |
| `api.grpc.maxSendMessageSize`   | `100KiB`         | Max outbound message size. |
| `api.grpc.keepaliveTimeout`     | `5s`             | Server keepalive timeout. |

## cluster

| Key                              | Default         | Description |
| -------------------------------- | --------------- | ----------- |
| `cluster.enabled`                | `false`         | Enable Raft replication; `false` = standalone node writing directly to storage. |
| `cluster.nodeId`                 | `1`             | Unique Raft node ID. Must be nonzero when clustering is enabled. |
| `cluster.shardId`                | `1`             | Event-shard ID. Must be nonzero. |
| `cluster.joinSeeds`              | `[]`            | gRPC addresses of existing nodes to contact for `JoinCluster`. Mutually exclusive with `initialMembers`. |
| `cluster.raft.listen`            | `0.0.0.0:50005` | Dragonboat Raft listen address. |
| `cluster.raft.advertise`         | `""`            | Raft address advertised to peers. Non-wildcard when clustering. |
| `cluster.raft.dataDir`           | `./raft-data`   | Dragonboat LogDB / node-host data directory. |
| `cluster.raft.initialMembers`    | `{}`            | Bootstrap map `nodeId → raftAddress` for a fresh cluster. If set, it must contain this node's own ID mapped to its advertise address. |
| `cluster.raft.rtt`               | `200ms`         | Estimated inter-node round-trip time driving Dragonboat's timers. Whole milliseconds, ≥1ms. |
| `cluster.raft.snapshotEntries`   | `10000`         | Take a state-machine snapshot after this many applied log entries. |
| `cluster.raft.compactionOverhead`| `5000`          | Log entries retained above the snapshot index. |
| `cluster.raft.logDB` | See below | Dragonboat LogDB settings and persistence worker count. |

### cluster.raft.logDB

All fields of Dragonboat's `LogDBConfig` are configurable in this block. Byte
quantities use `B`, `KiB`, `MiB`, or `GiB`; optional byte settings accept `0B`.
Environment overrides follow the nesting, for example
`FUTUREQ_CLUSTER_RAFT_LOGDB_KVWRITEBUFFERSIZE=4MiB` and
`FUTUREQ_CLUSTER_RAFT_LOGDB_WORKERS=4`.

`workers` controls `Engine.ExecShards`, whose workers persist Raft updates.
`shards` controls `LogDBConfig.Shards`. Changing either count requires fresh
Raft storage; Dragonboat rejects incompatible saved partition settings.

FutureQ uses the Tan backend, which directly consumes `kvWriteBufferSize` and
retains **16 buffers regardless of the worker setting**. The `16MiB` default
therefore retains **256MiB per broker**. Oversized updates use temporary
allocations. Other KV/cache/level options are passed through to Dragonboat but
belong to its other LogDB backends and have no effect on Tan. These settings
are separate from application storage under `storage.pebble`.

| Key below `cluster.raft.logDB` | Default |
|---|---:|
| `workers` | `16` |
| `shards` | `16` |
| `kvKeepLogFileNum` | `16` |
| `kvMaxBackgroundCompactions` | `2` |
| `kvMaxBackgroundFlushes` | `2` |
| `kvLRUCacheSize` | `0B` |
| `kvWriteBufferSize` | `16MiB` |
| `kvMaxWriteBufferNumber` | `4` |
| `kvLevel0FileNumCompactionTrigger` | `8` |
| `kvLevel0SlowdownWritesTrigger` | `17` |
| `kvLevel0StopWritesTrigger` | `24` |
| `kvMaxBytesForLevelBase` | `4GiB` |
| `kvMaxBytesForLevelMultiplier` | `2` |
| `kvTargetFileSizeBase` | `16MiB` |
| `kvTargetFileSizeMultiplier` | `2` |
| `kvLevelCompactionDynamicLevelBytes` | `0` |
| `kvRecycleLogFileNum` | `0` |
| `kvNumOfLevels` | `7` |
| `kvBlockSize` | `32KiB` |
| `saveBufferSize` | `32KiB` |
| `maxSaveBufferSize` | `64MiB` |

## storage

| Key                            | Default      | Description |
| ------------------------------ | ------------ | ----------- |
| `storage.engine`               | `pebble`     | `pebble` (LSM) or `bolt` (single-file B+-tree). |
| `storage.virtualShards`         | `256`        | Number of virtual delivery shards for new clustered publishes. Accepts powers of two from `1` through `65536`. The event leader reads this at startup. See [virtual delivery shards](storage.md#virtual-delivery-shards). |
| `storage.pebble.mode`          | `disk`       | `disk` or `memory` (Pebble over an in-memory FS; nothing persists). |
| `storage.pebble.dataDir`       | `./data`     | Pebble data directory. |
| `storage.pebble.walEnabled`    | `true`       | Pebble WAL toggle. Automatically disabled in memory mode and in clustered mode (the Raft log is the WAL); may only be set `false` explicitly for clustered disk nodes. |
| `storage.pebble.cacheSize`     | `16MiB`      | Block cache size. |
| `storage.pebble.memtableSize`  | `64MiB`      | Memtable size. |
| `storage.bolt.file`            | `./data/futureq.db` | bbolt database file (engine `bolt`). |
| `storage.bolt.bucket`          | `futureq`    | Single bbolt bucket holding the flat ordered keyspace. |

## publish

| Key                          | Default  | Description |
| ---------------------------- | -------- | ----------- |
| `publish.minAckLevel`        | `quorum` | Broker-wide minimum acknowledgement durability. One of `quorum`, `leader`, `noAck`. Publish batches requesting a weaker level are rejected with `InvalidArgument`. See [ack levels](clustering.md#publish-ack-levels). |
| `publish.proposalTimeout`    | `5s`     | Upper bound for each Raft propose / leader-ack wait. Must be positive. |

## delivery

| Key                                 | Default | Description |
| ----------------------------------- | ------- | ----------- |
| `delivery.timeBucket`               | `1ms`   | Granularity of the key scheme's time buckets. `0` = raw-millisecond buckets (maximum precision). Otherwise must be ≥1ms. |
| `delivery.heapType`                 | `binary` | Candidate heap for due and future messages: `binary` (typed binary), `4ary`, or `8ary`. Applied at startup; restart to change. All choices order by exact due time, then message key. |
| `delivery.shardDistribution`        | `consumerCount` | Per-group cluster delivery policy. `consumerCount` weights replicas by their consumer counts. `replicaCount` gives equal weight to replicas with at least one consumer in the group. Update the setting on all cluster nodes and restart them. The metadata leader activates its configured policy through a fenced rebalance. |
| `delivery.discoveryWorkers`          | `2`     | Concurrent topic discovery passes per node, `1..16`. |
| `delivery.shardWorkers`              | `1`     | Maximum concurrent shard scans within one sharded topic pass. Must be positive. Maximum total concurrency is `discoveryWorkers × shardWorkers`, subject to available shard ranges and the scan budget. A clustered node warns at startup when the configured product exceeds `GOMAXPROCS`. |
| `delivery.preloadScanIdleMaxInterval` | `100ms` | Maximum speculative idle scan backoff. `0` disables it; must be nonnegative. The effective limit is at least the poll interval. Payload mutations reset backoff; due repair cadence is unchanged. |
| `delivery.dispatchPollInterval`     | `5ms`   | Periodic live due-message scan interval, also used when preloading is enabled. |
| `delivery.preloadLookahead`         | `0s`    | How far ahead the scheduler scans for upcoming messages. Must be a nonnegative whole number of milliseconds. `0` disables preloading. |
| `delivery.preloadMaxMessages`       | `512`   | Maximum retained/reserved full-message future entries across topics, `0..1048576`. `0` disables future caching; bounded due delivery continues. Payload sizes determine memory use. |
| `delivery.inFlightTimeout`          | `5s`    | How long an unacknowledged delivery blocks redelivery; after it expires the message is re-dispatched (at-least-once). |
| `delivery.retryOwnerLease`          | `15s`   | How long a clustered broker keeps ownership of bounded delivery attempts after its last renewal. Renewed every fifth of the lease. Use positive whole milliseconds and at least three times `publish.proposalTimeout` in cluster mode. Environment override is `FUTUREQ_DELIVERY_RETRYOWNERLEASE`. |
| `delivery.deleteBatchInterval`      | `500ms` | How often the deleter flushes its batch of keys to delete (one Raft proposal per batch in clustered mode). |
| `delivery.deleteWorkers`            | `16` | Maximum concurrent ACK/delete flushes in Raft mode (1..256). Pending and in-progress completions share the existing queue bound. Standalone flushes remain serial. |
| `delivery.ackQueueCapacity`         | `8192` | Maximum unique pending message/recipient ACKs per broker (1..1048576), including in-progress persistence. Full queues reject new ACKs and close the consumer stream for retry. Explicit/TTL delete queue capacity is unchanged. |
| `delivery.receiptCache.maxBytes`     | `64MiB` | Estimated retained receipt bytes per standalone broker or Raft replica, excluding the slot table. `0B` disables the receipt cache. |
| `delivery.receiptCache.maxEntries`   | `524288` | Number of receipt slots per standalone broker or Raft replica. Accepts powers of two from 1 through 16,777,216. |
| `delivery.subscriptions.renewalWorkers` | `4` | Concurrent session renewal batches (1..16), at most 256 sessions per batch. |
| `delivery.subscriptions.allowAutoAck` | `false` | Allow workers to request `SubscribeInit.auto_ack`; otherwise reject them with `PermissionDenied` before binding. Manual ACK/NACK streams remain available. |
| `delivery.subscriptions.disconnectWorkers` | `1` | Concurrent session disconnect batches (1..16), at most 256 sessions per batch. |
| `delivery.consumerRemovalWorkers` | `1` | Concurrent Raft consumer membership removals (1..16), targeting unique disconnected stream IDs. |
| `delivery.deliveryWorkers`          | `2` | Delivery-worker concurrency (1..16), with at most one job per topic. Workers validate payloads, read receipts, check permissions, and queue sends. |
| `delivery.validationTimeout`        | `250ms` | Cooperative budget for payload validation and starting receipt reads, greater than zero and at most 1s. A successful receipt read already in progress can finish after the budget expires and proceed through delivery checks. Excludes final authorization, sender queueing, and gRPC sending. |
| `delivery.ttlsweepInterval`         | `60s`   | How often the TTL janitor sweeps the whole database for expired messages. |

All four intervals and the in-flight timeout must be greater than zero.

Receipt cache settings take effect at broker startup in standalone and clustered modes. Set
`FUTUREQ_DELIVERY_RECEIPTCACHE_MAXBYTES` and
`FUTUREQ_DELIVERY_RECEIPTCACHE_MAXENTRIES` for environment overrides. Slot blocks
allocate on demand and use up to about 20 MiB at the default entry limit on a
64-bit broker, in addition to the estimated receipt byte budget. The cache
evicts the oldest admitted receipts to fit new ones within the byte budget.
Storage supplies receipts on cache misses, after restart, and after Raft snapshot recovery.
Standalone also bounds tracking of in-flight publishes using both limits. A
publish batch still commits if it exceeds the available tracking capacity and
skips warming the cache in that case.

To compare delivery heaps, change only `delivery.heapType` between runs, or set
`FUTUREQ_DELIVERY_HEAPTYPE=8ary`. The heap is local to each broker and does not
change stored messages or the wire protocol. Both due-only delivery and future
preloading use the selected heap. See [heap comparisons](heap-scheduler.md#comparing-heaps)
for a repeatable microbenchmark; broker load tests are still needed to assess
delivery latency.

Auto-ack can also be enabled with
`FUTUREQ_DELIVERY_SUBSCRIPTIONS_ALLOWAUTOACK=true`. Apply the same policy on every
replica that accepts consumer streams. The default rejects auto-ack requests;
enabling it permits explicit opt-in and does not change existing streams.

Worker counts are applied at startup and require a restart to change. Environment
overrides for the new pools are `FUTUREQ_DELIVERY_SUBSCRIPTIONS_RENEWALWORKERS`,
`FUTUREQ_DELIVERY_SUBSCRIPTIONS_DISCONNECTWORKERS`, and
`FUTUREQ_DELIVERY_CONSUMERREMOVALWORKERS`. Disconnect and consumer-removal queues
remain bounded at 1024 entries each, shared by the workers. Each disconnect
worker can additionally hold one batch of at most 256 sessions; each removal
worker holds one command. Standalone writes still share the storage mutation
lock, so increasing these counts primarily helps Raft proposal waits.

Permit renewal remains one shared worker to coalesce reads. Topology publishing
and subscription expiry/receipt cleanup each retain one worker to serialize
epochs and own their scan cursors. Their worker counts are fixed.

`deliveryWorkers` and `validationTimeout` replace `prepareWorkers` and
`prepareTimeout`. Update YAML keys and environment overrides to
`FUTUREQ_DELIVERY_DELIVERYWORKERS` and `FUTUREQ_DELIVERY_VALIDATIONTIMEOUT`.

Preloading works in standalone and clustered mode. Configure it independently of
preparation batch sizes and quorum reads:

```yaml
delivery:
  preloadLookahead: 0s # opt-in trial: 100ms
  preloadMaxMessages: 512
```

The corresponding environment variables are `FUTUREQ_DELIVERY_PRELOADLOOKAHEAD`
and `FUTUREQ_DELIVERY_PRELOADMAXMESSAGES`. Either setting can be zero to disable
preloading. Increasing the lookahead or count does not change the scheduled due
time, freeze recipients early, or bypass a delivery read barrier. See the
[full-message cache sizing and resource budget](heap-scheduler.md#count-sizing-and-resource-budget)
before retaining an existing large count: this cache now owns payload data.

## observability

| Key                             | Default      | Description |
| ------------------------------- | ------------ | ----------- |
| `observability.logging.level`   | `info`       | zap log level. |
| `observability.metrics.listen`  | `0.0.0.0:9090` | HTTP address exposing Prometheus metrics. |

## Validation rules

- `configVersion` must be `1`.
- Advertised hosts may not be wildcard (`0.0.0.0` / `::`); listen addresses may.
- When `cluster.enabled`: `nodeId` and `shardId` must be nonzero, and `api.grpc.advertise` / `cluster.raft.advertise` must be set to reachable addresses.
- `cluster.raft.initialMembers` and `cluster.joinSeeds` are mutually exclusive.
- If `initialMembers` is set, it must include this node's own ID mapped to its own Raft advertise address.
- `cluster.raft.rtt` must be whole milliseconds ≥1ms.
- Unknown `publish.minAckLevel` values fail startup.
- `storage.pebble.walEnabled: false` is only legal for clustered disk nodes.

### delivery.subscriptions

| Key | Default | Meaning |
| --- | --- | --- |
| `delivery.subscriptions.disconnectTimeout` | `0s` | Duration without any connected worker before automatic unsubscribe and pending-delivery discard. `0s` retains group and universal subscriptions forever. Positive values must be whole milliseconds. Connected idle workers do not expire. |

Environment override: `FUTUREQ_DELIVERY_SUBSCRIPTIONS_DISCONNECTTIMEOUT=24h`.
Message TTL still applies. A never-connected subscription starts its timer at
creation; a crashed broker's connections are recognized through session lease
expiry before the disconnect timeout starts. Expiry and backlog cleanup are
asynchronous and bounded per maintenance pass.
