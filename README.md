# FutureQ v2

FutureQ is a high-performance, distributed delayed-message queue broker written in Go. It enables producers to publish messages with a relative delay, ensuring reliable dispatch to consumers when their delay expires.

## Project Purpose
FutureQ provides a reliable messaging backbone for modern distributed systems requiring delayed task execution. It solves the problem of scheduling and delivering delayed messages with strong consistency, durability, and high availability.

## Major Features
- **Delayed Messaging**: Enqueue messages to be delivered after a specific `delay_ms`.
- **Durable Storage**: Disk-backed storage using Pebble (embedded LSM store).
- **High Availability & Replication**: Raft consensus (Dragonboat) for data replication.
- **High Throughput**: Batch publishing and acknowledgements.
- **Consumer Groups & Topics**: Topic-based routing with fan-out across multiple consumer groups and round-robin dispatch within groups.
- **Message Expiry (TTL)**: Native support for message Time-To-Live.
- **Observability**: Prometheus metrics for deep visibility.

## Goals
- Guarantee at-least-once delivery of delayed messages.
- Provide a robust, highly available clustered broker.
- Maintain high throughput using batching and efficient storage structures.

## Non-Goals
- Exactly-once delivery (consumers must implement idempotency).
- Complex message transformations or routing rules.
- Long-term message archiving (messages are deleted upon consumption or expiry).

## Intended Users
- Platform engineers and software architects building distributed systems.
- Developers requiring reliable delayed task scheduling.

## Architecture Summary
FutureQ operates as a cluster of nodes with a single Raft Leader handling writes. Data is stored in Pebble using a time-optimized key schema. A dispatcher continuously scans for expired messages and routes them to connected consumers via gRPC. For a detailed view, see [Architecture](docs/ARCHITECTURE.md).

## Technology Stack
- **Language**: Go
- **Storage**: Pebble (CockroachDB's LSM tree)
- **Consensus**: Dragonboat (Raft)
- **Transport**: gRPC (Bidirectional streaming)
- **Membership**: HashiCorp Memberlist (Gossip protocol)
- **Observability**: Prometheus

## Quick Start
1. **Clone & Build**:
   ```bash
   git clone https://github.com/futureq-io/futureq.git
   cd futureq
   go build -o futureq ./internal/main.go
   ```
2. **Configure**:
   Copy `config.example.yaml` to `config.yaml` and adjust as needed.
3. **Run**:
   ```bash
   ./futureq start --config config.yaml
   ```

## Development Workflow
See [Development](docs/DEVELOPMENT.md) for local setup, testing, and CI/CD pipelines.
See [Coding Guidelines](docs/CODING_GUIDELINES.md) for style and architectural rules.

## Deployment Overview
FutureQ is deployed as a StatefulSet in Kubernetes using Helm, with gossip-based peer discovery. See [Deployment](docs/DEPLOYMENT.md) for details.

## Wiki Directory
- [Project Overview](docs/PROJECT_OVERVIEW.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Directory Structure](docs/DIRECTORY_STRUCTURE.md)
- [File Reference](docs/FILE_REFERENCE.md)
- [Components](docs/COMPONENTS.md)
- [API Reference](docs/API.md)
- [Database Schema](docs/DATABASE.md)
- [Configuration](docs/CONFIGURATION.md)
- [Dependencies](docs/DEPENDENCIES.md)
- [Development](docs/DEVELOPMENT.md)
- [Coding Guidelines](docs/CODING_GUIDELINES.md)
- [Security](docs/SECURITY.md)
- [Testing](docs/TESTING.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)
- [Performance](docs/PERFORMANCE.md)
- [Deployment](docs/DEPLOYMENT.md)
- [Architecture Decisions (ADRs)](docs/DECISIONS.md)
- [Glossary](docs/GLOSSARY.md)
- [AI Context](docs/AI_CONTEXT.md)
- [Changelog](CHANGELOG.md)
- [Future Roadmap](docs/FUTURE.md)
