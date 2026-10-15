# Observability

## Prometheus metrics

Metrics are exposed in Prometheus format at `observability.metrics.listen` (default `0.0.0.0:9090`). The metrics server starts *before* Raft so it can serve readiness/liveness probes during a slow bootstrap.

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
| `messages_dispatched_total`   | counter   | —      | Messages handed to consumers |
| `messages_in_flight`          | gauge     | —      | Currently dispatched-but-unacked messages |
| `dispatch_pass_duration_ms`   | histogram | —      | Duration of a dispatcher scan pass |
| `delivery_latency_ms`         | histogram | —      | Now − enqueue time (clamped at 0 for clock skew) |
| `delivery_overhead_ms`        | histogram | —      | Now − scheduled fire time (clamped at 0) |
| `messages_expired_total`     | counter   | topic, reason (`dispatcher\|janitor`) | TTL expirations, by which path caught them |
| `active_consumers`            | gauge     | topic, group | Connected consumers |
| `consumer_ack_total`          | counter   | topic, group, success | ACK/NACK frames received |
| `delete_batch_size`           | histogram | —      | Keys per delete flush |
| `delete_failures_total`      | counter   | —      | Failed delete flushes (retried) |

## Logging

Structured logging via [zap](https://github.com/uber-go/zap); level configurable with `observability.logging.level` (default `info`) or `FUTUREQ_OBSERVABILITY_LOGGING_LEVEL`.
