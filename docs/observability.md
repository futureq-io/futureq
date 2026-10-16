# Observability

## Prometheus metrics

Metrics are exposed in Prometheus format at `observability.metrics.listen` (default `0.0.0.0:9090`). The metrics server starts *before* Raft so it can serve readiness/liveness probes during a slow bootstrap.

All application metric names below have the `futureq_` prefix. Duration and
lateness histograms use milliseconds.

### Publishing

| Metric                       | Type      | Labels | Meaning |
| ---------------------------- | --------- | ------ | ------- |
| `publish_requests_total`      | counter   | topic, ackLevel, result (`rejected\|error\|success`) | Publish batches by outcome. Rejections include ack-level violations. |
| `messages_published_total`   | counter   | topic  | Individual messages persisted |
| `publish_batch_size`         | histogram | —      | Messages per publish batch |
| `publish_latency_ms`         | histogram | —      | End-to-end publish latency |
| `raft_propose_duration_ms`   | histogram | ackLevel | Raft proposal latency per ack level |

!!! note

    Batches may span topics; the `topic` label joins distinct topics with `|`.

### Delivery

| Metric                        | Type      | Labels | Meaning |
| ----------------------------- | --------- | ------ | ------- |
| `messages_dispatched_total`   | counter   | topic, group_id | Successful handoffs to sender queues, once per recipient; includes retries. |
| `messages_in_flight`          | gauge     | topic, group_id | Active delivery attempts, including queued sends and successful ACKs awaiting persistence. Each group/key counts once; each universal recipient/key counts separately. |
| `dispatch_pass_duration_ms`   | histogram | —      | Successful preparation results processed through queueing, timed from candidate collection start; excludes speculative preload work and waiting for due time. Not a full topic pass. |
| `delivery_latency_ms`         | histogram | topic | Legacy enqueue-to-candidate-selection age, observed for successfully queued messages. Includes intentional producer delay; excludes preparation and sender-queue waiting. |
| `delivery_overhead_ms`        | histogram | topic | Candidate-selection-start time minus scheduled due time, clamped at zero, observed for successfully queued messages. Includes both live and cached selection. |
| `delivery_sender_enqueue_lateness_ms` | histogram | topic | Actual sender-enqueue time minus the individual due time, per successfully queued recipient attempt. Includes preparation; excludes sender-queue waiting. |
| `delivery_send_lateness_ms` | histogram | topic | Time immediately before each authorized gRPC Send attempt minus the individual due time. Includes sender-queue waiting, retries, and failed send attempts. |
| `delivery_grpc_sends_total` | counter | topic | Successful gRPC sends, including retries and fan-out. Not client acknowledgements. |
| `delivery_queue_depth` | gauge | queue | `consumer`: queued sender attempts across local subscriptions; `prepare`: outstanding jobs, not messages; `preload`: retained candidate keys, including future or stale entries. Not persistent backlog. |
| `delivery_oldest_queued_lateness_ms` | gauge | — | Oldest unsent tracked attempt's lateness, sampled by the scheduler. Excludes preload candidates. |
| `delivery_oldest_preparation_lateness_ms` | gauge | — | Oldest candidate due time in outstanding preparation jobs, sampled after scheduling. Zero when no due preparation job remains. |
| `delivery_scan_duration_ms` | histogram | — | Aggregate local duration of bounded collection operations across all four paths below, including empty operations. Excludes read barriers, preparation, and time resident in the preload cache. |
| `delivery_collection_duration_ms` | histogram | path | Duration by `due_scan`, `maintenance`, `preload_scan`, or `preload_ready`. `preload_ready` is cached candidate selection, before preparation. |
| `delivery_scanned_keys_total` | counter | — | Event-key observations in live and speculative preload storage scans, including repeated reads. Excludes cache-only selection and maintenance receipt scans. |
| `delivery_ownership_filtered_total` | counter | — | Event-key observations rejected for having no local owner, in live or speculative preload storage scans. |
| `delivery_candidates_total` | counter | source (`scan\|preload`) | Due candidates selected for preparation, including retries. Excludes future loads and maintenance. `source="preload"` counts cached candidate hits, not successful deliveries. |
| `delivery_preload_capacity` | gauge | — | Configured maximum retained candidates on this node. Zero when preloading is disabled or the scheduler stops. |
| `delivery_preload_loaded_total` | counter | — | New entries retained in the preload cache; duplicate scans do not increment it. Reloading after invalidation counts again. |
| `delivery_preload_scanned_keys_total` | counter | — | Speculative preload event-key observations; a subset of `delivery_scanned_keys_total`, not additional work to sum with it. |
| `messages_expired_total`     | counter   | topic, source (`dispatcher\|janitor`) | Accepted TTL deletion requests on this node, attributed to the first source. Repeated scans and failed flush retries are deduplicated while a deletion is pending. Not committed deletes or globally unique messages. |
| `active_consumers`            | gauge     | topic, group_id | Connected consumers |
| `consumer_ack_total`          | counter   | topic, group_id, success | ACK/NACK frames received |
| `delete_batch_size`           | histogram | —      | Keys per delete flush |
| `delete_failures_total`      | counter   | —      | Failed delete flushes (retried) |

### Interpreting preloading and readiness

Loading a future key is not a send attempt. It changes preload occupancy and
load counters, but does not change sender-enqueue lateness, send lateness,
dispatch counts, or in-flight records. Preparation, due-time checks, ownership,
TTL checks, and delivery permissions still apply before queueing.

Use `delivery_sender_enqueue_lateness_ms` to measure when a message becomes
ready for its sender, and `delivery_send_lateness_ms` for lateness immediately
before the gRPC send. The legacy selection-time histograms exclude preparation
and must not be described as end-to-end delivery latency. Percentiles are
independently sampled; subtracting stage percentiles does not measure stage time.

The aggregate collection histogram now includes preload scanning and cached
selection. For comparisons across this change, use the path histogram to
separate work; a lower live-scan rate alone does not establish lower total cost.
Gauges are node-local snapshots updated at scheduler boundaries. A retained
preload entry can be future-due or stale, so occupancy is not a count of messages
currently authorized to send.

For example, preload occupancy as a fraction of enabled capacity is:

```promql
futureq_delivery_queue_depth{queue="preload"}
  / ignoring(queue) (futureq_delivery_preload_capacity > 0)
```

The fraction of selected candidates supplied by preloading is:

```promql
sum(rate(futureq_delivery_candidates_total{source="preload"}[5m]))
  / (sum(rate(futureq_delivery_candidates_total[5m])) > 0)
```

These measure cache usage, not delivery success or a traditional cache-lookup
hit ratio. Apply matching job/pod filters when querying a shared Prometheus.

## Logging

Structured logging via [zap](https://github.com/uber-go/zap); level configurable with `observability.logging.level` (default `info`) or `FUTUREQ_OBSERVABILITY_LOGGING_LEVEL`.
