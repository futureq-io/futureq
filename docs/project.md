# Project layout

## Repository structure

```text
futureq/
├── cmd/futureq/            # Cobra CLI (start, leave)
├── internal/
│   ├── main.go              # entrypoint
│   ├── app/                 # wiring: storage, repositories, Raft lifecycle
│   ├── api/grpc/            # gRPC server + handlers (producer, consumer, cluster)
│   ├── config/              # config loading + env overrides + validation
│   ├── dispatcher/          # scan/dispatch loop, hub, deleter, TTL janitor
│   ├── metrics/             # Prometheus server
│   ├── raft/
│   │   ├── event/           # event-shard state machine & binary commands
│   │   └── leaderpersist/   # LogDB decorator + tracker for leader acks
│   ├── repository/          # event repository abstraction
│   └── storage/             # Pebble & bbolt engines behind a common contract
├── pkg/
│   ├── raft/metadata/       # metadata-group Raft (public: cluster membership)
│   ├── log/                 # zap logger setup
│   └── utils/               # 24-byte event key helpers
├── config.example.yaml      # documented defaults
├── Dockerfile
└── CHANGELOG.md
```

`pkg/raft/metadata` is deliberately public so external SDKs can join the metadata group as read-only observers and receive live topology updates.

## Development

```bash
go build ./...       # build
go test ./...        # run tests (CI runs with -race)
golangci-lint run    # lint
```

## CI (GitHub Actions, on every push)

| Workflow           | What it runs |
| ------------------ | ------------ |
| `build.yml`        | `go mod download` + `go build -v ./...` |
| `test.yml`         | `go test -race -v ./...` |
| `golangci-lint.yml`| golangci-lint action (latest version, read-only permissions) |

All on ubuntu-latest with Go 1.26.5.

## Contributing

Contributions are welcome — please open an issue to discuss substantial changes before sending a PR. FutureQ is released under the [MIT license](https://github.com/futureq-io/futureq/blob/main/LICENSE).
