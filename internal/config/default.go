package config

import "time"

var defaultConfig = Config{
	Server: Server{
		Listen:        "0.0.0.0:8443",
		MaxConns:      10,
		Timeout:       5 * time.Second,
		MaxSendSizeKB: 100,
		MaxRecvSizeKB: 100,
	},

	Observability: Observability{
		Logger: Logger{
			Level: "info",
		},
		Metrics: Metrics{
			Addr: "0.0.0.0:9090",
		},
	},

	Storage: Storage{
		MinAckLevel:    Quorum,
		Persist:        true,
		TimeBucketSize: 1 * time.Millisecond,
		Pebble: Pebble{
			DisableWAL:       false,
			DataPath:         "./data",
			CacheSizeMB:      16,
			InMemTableSizeMB: 64,
		},
	},

	Raft: Raft{
		Enabled:            false,
		NodeID:             1,
		ClusterID:          1,
		ListenAddress:      "0.0.0.0:50005",
		DataPath:           "./raft-data",
		InitialMembers:     map[uint64]string{1: "0.0.0.0:50005"},
		RTTMillisecond:     200,
		SnapshotEntries:    10000,
		CompactionOverhead: 5000,
	},

	Consumer: Consumer{
		DispatchPollIntervalMs: 50,
		DeleteBatchIntervalMs:  500,
		InFlightTimeoutMs:      5000,
		TTLJanitorIntervalMs:   60000,
	},
}
