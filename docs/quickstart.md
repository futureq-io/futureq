# Quick start

## Prerequisites

- Go 1.26+
- (Optional) Docker

## Build & run a standalone node

```bash
git clone https://github.com/futureq-io/futureq.git
cd futureq
go build -o futureq ./cmd/futureq

cp config.example.yaml config.yaml   # adjust as needed
./futureq start -c config.yaml
```

A standalone node (`cluster.enabled: false`) writes directly to Pebble with a synchronous fsync per publish batch — perfect for local development. See the [gRPC API](api.md) page for how to talk to it.

## Run a 3-node cluster

On the **first node**, enable clustering and list its bootstrap member. Use addresses that other nodes and clients can reach:

```yaml
api:
  grpc:
    advertise: "10.0.0.1:8443"
cluster:
  enabled: true
  nodeId: 1
  shardId: 1
  raft:
    advertise: "10.0.0.1:50005"
    initialMembers:
      1: "10.0.0.1:50005"
```

For each **additional node**, give it a unique `cluster.nodeId`, its own `api.grpc.advertise` and `cluster.raft.advertise`, and a seed address. For example, node 2 uses:

```yaml
api:
  grpc:
    advertise: "10.0.0.2:8443"
cluster:
  enabled: true
  nodeId: 2
  raft:
    advertise: "10.0.0.2:50005"
  joinSeeds:
    - "10.0.0.1:8443"
```

The `--join` flag overrides `cluster.joinSeeds` for one start:

```bash
./futureq start -c node2.yaml --join 10.0.0.1:8443
./futureq start -c node3.yaml --join 10.0.0.1:8443
```

!!! note "Join flow"

    On first start the joining node contacts each seed until one accepts its `JoinCluster` request. Membership is registered on *both* the event shard and the metadata group — first as a non-voting member that catches up, then promoted to voter. Restarts detect local Raft data and skip the join flow automatically. `cluster.raft.initialMembers` and `cluster.joinSeeds` cannot both be set in a config file.

## Docker

```bash
docker build -t futureq .
docker run -p 8443:8443 -p 9090:9090 -p 50005:50005 \
  -v $(pwd)/config.yaml:/app/config.yaml \
  futureq start -c /app/config.yaml
```
