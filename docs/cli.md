# CLI

The binary is built with [Cobra](https://cobra.dev). Both subcommands share the persistent `--config / -c` flag.

## `futureq start`

```bash
futureq start [-c config.yaml] [--join seed:8443 ...]
```

- Loads and validates the config (file + `FUTUREQ_*` env).
- Initializes storage and the event repository.
- `--join` (repeatable) overrides `cluster.joinSeeds` for this start. Used only on a fresh start (no local Raft data); restarts skip the join flow.
- Builds the hub, deleter (Raft delete backend in clustered mode, direct backend in standalone), and TTL janitor.
- Starts the Prometheus metrics server *before* Raft, then the Raft groups (if enabled), then the gRPC server.
- Waits for `SIGTERM`/`SIGINT`; shuts down gracefully with a 10s window — components are WaitGroup-tracked; gRPC gets `GracefulStop` with a hard `Stop` fallback if the deadline expires.

## `futureq leave`

```bash
futureq leave --seed <host:8443> [-c config.yaml]
```

- Calls `LeaveCluster` on the seed with the configured `cluster.nodeId` (30s timeout).
- `--seed` is required; fails if clustering is disabled or the RPC fails.
- After a successful leave, the node's local Raft data directory can be deleted.

## Exit codes

Configuration validation errors, failed joins, and failed leaves exit non-zero with a log message; `start` exits 0 after a clean signal-triggered shutdown.
