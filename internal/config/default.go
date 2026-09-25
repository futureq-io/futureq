package config

import "time"

var defaultConfig = Config{
	ConfigVersion: 1,
	API: API{GRPC: GRPC{
		Listen: "0.0.0.0:8443", Advertise: "", MaxConcurrentStreams: 10,
		MaxReceiveMessageSize: "100KiB", MaxSendMessageSize: "100KiB", KeepaliveTimeout: 5 * time.Second,
	}},
	Cluster: Cluster{
		Enabled: false, NodeID: 1, ShardID: 1, JoinSeeds: []string{},
		Raft: Raft{
			Listen: "0.0.0.0:50005", Advertise: "", DataDir: "./raft-data",
			InitialMembers: map[uint64]string{}, RTT: 200 * time.Millisecond,
			SnapshotEntries: 10000, CompactionOverhead: 5000,
		},
	},
	Storage: Storage{
		Engine: "pebble",
		Pebble: Pebble{Mode: "disk", DataDir: "./data", WALEnabled: true, CacheSize: "16MiB", MemtableSize: "64MiB"},
		Bolt:   Bolt{File: "./data/futureq.db", Bucket: "futureq"},
	},
	Publish: Publish{MinAckLevel: Quorum, ProposalTimeout: 5 * time.Second},
	Delivery: Delivery{
		TimeBucket:           time.Millisecond,
		DispatchPollInterval: 50 * time.Millisecond,
		InFlightTimeout:      5 * time.Second,
		DeleteBatchInterval:  500 * time.Millisecond,
		TTLSweepInterval:     time.Minute,
	},
	Observability: Observability{Logging: Logger{Level: "info"}, Metrics: Metrics{Listen: "0.0.0.0:9090"}},
}
