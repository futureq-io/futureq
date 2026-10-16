# Full-message scheduler evaluation protocol

Acceptance criteria fixed before running the new end-to-end comparison:

- Correctness and race suites, plus ten repetitions of forced leader loss, pass.
- Valid cache hits perform zero final-delivery payload Gets/decodes. Preparation
  existence reads and receipt reads are reported separately.
- Compare original metadata-only, original disabled-preload, new full-message,
  and new due-only modes, at identical offered load and database contents.
- Use three runs. Differences below 10% in CPU/allocations or 20% in p99 lateness
  are inconclusive on this shared laptop. Scheduled p99 must stay within 20% of
  baseline plus 2 ms; immediate throughput/CPU regressions above 10% prevent
  enabling speculative full-message preloading by default without more evidence.
- Count admission stays bounded under blocked scans, failed/full sends, and
  sparse drain. Check retained memory against payload size plus object overhead.
- These local embedded broker workloads include publish persistence, discovery,
  manifest preparation, queueing, send authorization, recipient ACK persistence
  and delete. They do not include gRPC/TLS or external network latency; they
  cannot establish production broker throughput.

Results and rollout decision are appended after execution. No external benchmark
cluster is used.

## Environment and scope

2026-10-16, macOS arm64, Apple M3, Go 1.27.1. Original source is
`cd2109f3b7446940ded70fb1012268a911634ae0`, unpacked into a disposable directory.
The same `scheduler_workload_test.go` runs against both versions. No external
cluster was accessed. Three repetitions of each of 60 core variants (180 runs),
plus 12 retry/slow variants (36 runs) and 8 idle variants (24 runs).

Each non-idle run publishes 1200 messages at an offered 5000/s in 16-message
synchronously committed batches, with the same keys, deterministic payloads,
consumer counts, ACK/delete behavior and storage engine settings. Future-enabled
comparisons use a 1-second lookahead and 512 entries on both versions to stress
retention; they do not increase the deployment recommendation beyond a 100 ms
opt-in trial. Cases are immediate 4 KiB, 100 ms delayed 1 KiB or 64 KiB, 200 ms
delayed mixed 1/64 KiB, and 16 topics with 50 ms delayed 4 KiB messages.
Mixed payloads use a 1:7 ratio of large to small messages. Publish persistence,
all speculative/repair scanning, preparation, current-receipt checks, enqueue,
send authorization and ACK/delete persistence are included. Send time is sampled
at the local consumer before its ACK; there is no gRPC/TLS/network simulation.
Enqueue percentiles are histogram bucket upper bounds, not exact quantiles.
Throughput includes the intentional delay tail, and the rate is offered load,
not a claim that every disk engine sustained that publish rate. CPU is process
user+system time. Allocation and sampled peak heap cover the entire workload,
including the storage engine; they are not cache-only memory measurements.

The laptop and filesystem introduce substantial variance, especially in disk
and many-topic tails. The CSV retains every run so this variance is visible.
The new scheduler avoids final payload rereads on valid hits but retains fresh
receipt reads. Extra coordination, heap admission and receipt decoding cost CPU
for some small/immediate/many-topic workloads. These runs do not establish a
universal efficiency improvement.

## Core results

Medians of three runs. `M` is original metadata-only preloading; `F` is the new
full-message future cache. CPU and allocation changes are F relative to M.

| Engine | Case | M/F messages/s | CPU change | Allocation change | M/F send p99 ms | F payload Gets/message |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| pebble-memory | immediate | 5044 / 4979 | +7% | -8% | 12.4 / 8.6 | 1.00 |
| pebble-memory | scheduled-small | 3566 / 3561 | -19% | -4% | 1.7 / 1.0 | 1.11 |
| pebble-memory | scheduled-large | 3527 / 3346 | +2% | -6% | 6.4 / 25.4 | 1.09 |
| pebble-memory | mixed-overflow | 2747 / 2748 | -30% | -7% | 2.1 / 1.2 | 1.16 |
| pebble-memory | many-topics | 4175 / 4162 | +43% | +4% | 1.0 / 2.5 | 1.10 |
| pebble-disk | immediate | 2845 / 2731 | +31% | +35% | 48.6 / 45.6 | 1.00 |
| pebble-disk | scheduled-small | 2378 / 2732 | +39% | +100% | 58.7 / 36.0 | 1.15 |
| pebble-disk | scheduled-large | 499 / 1667 | -48% | -11% | 1574.0 / 58.5 | 1.05 |
| pebble-disk | mixed-overflow | 2369 / 2158 | +2% | -9% | 37.7 / 29.1 | 1.20 |
| pebble-disk | many-topics | 2229 / 2436 | +241% | +433% | 249.1 / 385.9 | 1.16 |
| bolt | immediate | 1383 / 1419 | -14% | -7% | 92.1 / 88.5 | 1.01 |
| bolt | scheduled-small | 1502 / 1367 | +15% | +59% | 58.2 / 63.7 | 1.05 |
| bolt | scheduled-large | 386 / 1268 | -60% | -34% | 2101.8 / 59.0 | 1.03 |
| bolt | mixed-overflow | 1324 / 1343 | -4% | -13% | 51.1 / 67.5 | 1.10 |
| bolt | many-topics | 1145 / 1200 | +41% | +80% | 515.9 / 773.0 | 1.11 |

Full-message delivery uses approximately one payload Get/message for the first
manifest's existence check, versus approximately two in the original path.
The amount above one is conservative fallback/recovery work, including stripe
collisions or a mutation overlapping snapshot creation. A focused test verifies
zero **final** payload Gets/decodes for valid cached data. Receipt preparation,
ACK persistence and the final current-receipt check remain visible metadata
reads in the CSV. Iterator values are storage work too; Get counts alone do not
represent total storage cost. Discovery counters expose examined bytes/keys.

Both original no-preload and new due-only cases are included in the raw results:

| Engine | Case | Original/new due-only CPU seconds | Original/new send p99 ms | New payload Gets/message |
| --- | --- | ---: | ---: | ---: |
| pebble-memory | immediate | 0.187 / 0.149 | 3.5 / 12.9 | 1.00 |
| pebble-memory | scheduled-large | 0.311 / 0.307 | 7.2 / 13.6 | 1.00 |
| pebble-memory | many-topics | 0.225 / 0.220 | 5.0 / 10.2 | 1.00 |
| pebble-disk | immediate | 0.088 / 0.112 | 37.9 / 43.2 | 1.01 |
| pebble-disk | scheduled-large | 0.444 / 0.255 | 1472.6 / 61.9 | 1.00 |
| pebble-disk | many-topics | 0.106 / 0.135 | 162.2 / 635.8 | 1.05 |
| bolt | immediate | 0.134 / 0.150 | 98.0 / 87.3 | 1.03 |
| bolt | scheduled-large | 0.720 / 0.249 | 3267.8 / 116.8 | 1.02 |
| bolt | many-topics | 0.162 / 0.277 | 353.9 / 1320.8 | 1.06 |

## Slow paths, retries and idle operation

Extra Pebble-memory runs use the same 1200 messages with (a) every fifth first
attempt NACKed and ACK persistence every 20 ms, (b) 10 ms preparation latency,
or (c) a 2 ms slower consumer on one of 16 topics. Each is repeated three times
in all four version/preload combinations. NACK runs produce 240 intentional
retries; unexpected duplicates are zero in all measured runs. The committed CSV
contains both send and enqueue percentiles, CPU, allocation, heap and reads for
these cases; sender full-queue/backoff is also forced in correctness tests.

| Mode | Extra case | M/F send p99 ms | M/F CPU seconds |
| --- | --- | ---: | ---: |
| preload | retry-slow-ack | 2.3 / 9.3 | 0.134 / 0.158 |
| preload | slow-prepare | 54.6 / 24.2 | 0.148 / 0.132 |
| preload | slow-topic | 2.9 / 2.8 | 0.235 / 0.301 |
| due-only | retry-slow-ack | 9.2 / 19.9 | 0.088 / 0.173 |
| due-only | slow-prepare | 32.1 / 31.8 | 0.096 / 0.060 |
| due-only | slow-topic | 4.9 / 8.8 | 0.107 / 0.099 |

Idle runs last 500 ms each, with either no subscribers or one empty subscribed
topic. CPU medians and total iterator counts below demonstrate bounded polling;
there is no empty-heap busy loop.

| Mode | Empty subscribed topics | M/F CPU seconds | M/F range scans |
| --- | ---: | ---: | ---: |
| preload | 0 | 0.0131 / 0.0138 | 59 / 57 |
| preload | 1 | 0.0115 / 0.0174 | 258 / 168 |
| due-only | 0 | 0.0115 / 0.0043 | 58 / 57 |
| due-only | 1 | 0.0107 / 0.0156 | 159 / 116 |

A separate three-replica asynchronous scheduled-delivery test verifies named
ownership and universal follower fan-out. This is correctness coverage, not a
Raft throughput benchmark. Tests preserve forced transfer of event leadership to
replica 3 before its failure, including bounded transient subscription retries.

## Retained memory and rollout

At 512 messages, allocation profiles for the decode/cache call path measured
594728 bytes at 1 KiB, 33624872 bytes at 64 KiB and 54597192 bytes at 100 KiB
(about 0.57, 32.07 and 52.07 MiB), plus heap/map bookkeeping. Candidate and decoded
message structs are 72 and 128 bytes. Payload allocator rounding explains the
extra approximately 2 MiB for 100 KiB messages. Whole-process GC heap deltas are
noisy (some negative after storage garbage collection), so the report uses
allocation stacks and owned payload counts rather than presenting those deltas
as exact cache memory. The sizing test also emits the raw deltas for inspection.

The design documentation gives the separate scan, ready, assigned, sender,
transient and validity-metadata bounds. Whole-workload peak heap in the CSV may
be much larger because it includes storage buffers and GC slack. No count cap
is advertised as a hard byte cap.

The acceptance criteria are **not met for enabling future speculation by
default**: some immediate/many-topic CPU and scheduled tail measurements regress.
Future preloading therefore defaults to zero lookahead, with 512 as the configured
count when opted in. Try 100 ms / 512 on a measured scheduled deployment.
Explicit existing lookaheads/counts are honoured, but should be resized because
they now retain payloads. The new due-only heap path still reuses full messages;
it also has measurable coordination/current-receipt overhead on some engines.
Do not infer a production-wide gain from the large-message disk improvements.

## Reproduction and validation

```sh
GOCACHE=/tmp/futureq-go-cache go test ./...
GOCACHE=/tmp/futureq-go-cache go test -race -v ./...
GOCACHE=/tmp/futureq-go-cache go test -race ./internal/dispatcher \
  -run '^TestReplicaClusterTimeoutOfflineRebalanceAndRestart$' -count=10
GOCACHE=/tmp/futureq-go-cache GOLANGCI_LINT_CACHE=/tmp/futureq-lint-cache golangci-lint run
GOCACHE=/tmp/futureq-go-cache go build -mod=vendor -buildvcs=false ./...
git diff --check
node .gitnexus/run.cjs detect-changes --scope all --limit 1000 --repo .
FUTUREQ_CACHE_MEMORY=1 go test ./internal/dispatcher -run '^TestFullMessageCacheSizing$' -v -count=3
FUTUREQ_WORKLOAD=1 FUTUREQ_WORKLOAD_MODE=preload \
  FUTUREQ_WORKLOAD_ENGINE=pebble-memory FUTUREQ_WORKLOAD_SIZE=65536 \
  FUTUREQ_WORKLOAD_DELAY_MS=100 go test ./internal/dispatcher \
  -run '^TestSchedulerWorkload$' -v -count=3
```

Use `due-only`, `pebble-disk` or `bolt` to vary the core matrix. Size 0 selects
mixed payloads. `FUTUREQ_WORKLOAD_TOPICS=16` selects many topics. Extra knobs are
`FUTUREQ_WORKLOAD_NACK_EVERY`, `FUTUREQ_WORKLOAD_ACK_MS`,
`FUTUREQ_WORKLOAD_PREPARE_MS` and `FUTUREQ_WORKLOAD_SLOW_CONSUMER_MS`.
`FUTUREQ_IDLE_WORKLOAD=1` runs `TestSchedulerIdleWorkload`, with
`FUTUREQ_IDLE_ACTIVE=1` for the empty subscription. Copy the workload file to the
original source tree to repeat the baseline; it deliberately uses the common
surface of both versions. Socket tests require local loopback access.

The baseline race suite and ten forced-leader repetitions passed. The revised
suite additionally covers payload-read avoidance, pending/failed mutation proofs,
actual snapshot replacement, delayed deleted scan results, committed partial
ACKs, old subscription jobs, zero settings, blocked scans, sparse backing-array
release and asynchronous Raft fan-out. Lint and build checks are required at the
reviewed head. GitNexus reports critical graph impact across the storage and
delivery flows; it is an affected-path audit, not a proof of interface completeness.

Raw measurements: [all 216 workload runs](heap-scheduler-workloads.csv).
