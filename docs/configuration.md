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

## storage

| Key                            | Default      | Description |
| ------------------------------ | ------------ | ----------- |
| `storage.engine`               | `pebble`     | `pebble` (LSM) or `bolt` (single-file B+-tree). |
| `storage.payloadCacheSlots`    | `4096`       | Shared payload freshness slots per DB, for both engines and standalone/Raft. Power of two from `64` to `1048576`; requires restart. Larger counts reduce conservative cache invalidations from hash collisions. |
| `storage.pebble.mode`          | `disk`       | `disk` or `memory` (Pebble over an in-memory FS; nothing persists). |
| `storage.pebble.dataDir`       | `./data`     | Pebble data directory. |
| `storage.pebble.walEnabled`    | `true`       | Pebble WAL toggle. Automatically disabled in memory mode and in clustered mode (the Raft log is the WAL); may only be set `false` explicitly for clustered disk nodes. |
| `storage.pebble.cacheSize`     | `16MiB`      | Block cache size. |
| `storage.pebble.memtableSize`  | `64MiB`      | Memtable size. |
| `storage.bolt.file`            | `./data/futureq.db` | bbolt database file (engine `bolt`). |
| `storage.bolt.bucket`          | `futureq`    | Single bbolt bucket holding the flat ordered keyspace. |

For example, set `storage.payloadCacheSlots: 16384` or
`FUTUREQ_STORAGE_PAYLOADCACHESLOTS=16384`. This changes freshness tracking, not the
number of cached messages or Pebble's block cache. At `S` slots the tracking
arrays use approximately `12*S` bytes per DB. Batches touching message keys
collect a sparse list of `uint32` slots, sorted and deduplicated before taking the
tracker lock. Commit traversal depends on touched slots rather than table size;
metadata-only batches allocate no tracking list. Benchmark the cache-hit rate and delivery latency before
increasing it substantially. Nodes may use different counts; this setting changes
no stored records or wire format.

## publish

| Key                          | Default  | Description |
| ---------------------------- | -------- | ----------- |
| `publish.minAckLevel`        | `quorum` | Broker-wide minimum acknowledgement durability. One of `quorum`, `leader`, `noAck`. Publish batches requesting a weaker level are rejected with `InvalidArgument`. See [ack levels](clustering.md#publish-ack-levels). |
| `publish.proposalTimeout`    | `5s`     | Upper bound for each Raft propose / leader-ack wait. Must be positive. |

## delivery

| Key                                 | Default | Description |
| ----------------------------------- | ------- | ----------- |
| `delivery.timeBucket`               | `1ms`   | Granularity of the key scheme's time buckets. `0` = raw-millisecond buckets (maximum precision). Otherwise must be ≥1ms. |
| `delivery.discoveryWorkers`          | `2`     | Independent storage discovery workers, `1..16`; at most one scan per topic. |
| `delivery.preloadScanIdleMaxInterval` | `100ms` | Maximum speculative idle scan backoff. `0` disables it; must be nonnegative. The effective limit is at least the poll interval. Payload mutations reset backoff; due repair cadence is unchanged. |
| `delivery.dispatchPollInterval`     | `5ms`   | Periodic live due-message scan interval, also used when preloading is enabled. |
| `delivery.preloadLookahead`         | `0s`    | How far ahead the scheduler scans for upcoming messages. Must be a nonnegative whole number of milliseconds. `0` disables preloading. |
| `delivery.preloadMaxMessages`       | `512`   | Maximum retained/reserved full-message future entries across topics, `0..1048576`. `0` disables future caching; bounded due delivery continues. Payload sizes determine memory use. |
| `delivery.inFlightTimeout`          | `5s`    | How long an unacknowledged delivery blocks redelivery; after it expires the message is re-dispatched (at-least-once). |
| `delivery.deleteBatchInterval`      | `500ms` | How often the deleter flushes its batch of keys to delete (one Raft proposal per batch in clustered mode). |
| `delivery.ttlsweepInterval`         | `60s`   | How often the TTL janitor sweeps the whole database for expired messages. |

All four intervals and the in-flight timeout must be greater than zero.

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
