package config

import (
	"time"

	"github.com/futureq-io/futureq/pkg/raft/metadata"
)

var defaultConfig = Config{
	ConfigVersion: 1,
	API: API{GRPC: GRPC{
		Listen: "0.0.0.0:8443", Advertise: "", MaxConcurrentStreams: 10,
		MaxReceiveMessageSize: "100KiB", MaxSendMessageSize: "100KiB", KeepaliveTimeout: 5 * time.Second,
		MaxConnectionIdle:     30 * time.Second,
		MaxConnectionAge:      0 * time.Second,
		MaxConnectionAgeGrace: 0 * time.Second,
	}},
	Cluster: Cluster{
		Enabled: false, NodeID: 1, ShardID: 1, JoinSeeds: []string{},
		Raft: Raft{
			Listen: "0.0.0.0:50005", Advertise: "", DataDir: "./raft-data",
			InitialMembers: map[uint64]string{}, RTT: 200 * time.Millisecond,
			SnapshotEntries: 10000, CompactionOverhead: 5000,
			LogDB: LogDB{
				Workers: 16, Shards: 16, KVKeepLogFileNum: 16,
				KVMaxBackgroundCompactions: 2, KVMaxBackgroundFlushes: 2,
				KVLRUCacheSize: "0B", KVWriteBufferSize: "16MiB", KVMaxWriteBufferNumber: 4,
				KVLevel0FileNumCompactionTrigger: 8, KVLevel0SlowdownWritesTrigger: 17, KVLevel0StopWritesTrigger: 24,
				KVMaxBytesForLevelBase: "4GiB", KVMaxBytesForLevelMultiplier: 2,
				KVTargetFileSizeBase: "16MiB", KVTargetFileSizeMultiplier: 2,
				KVLevelCompactionDynamicLevelBytes: 0, KVRecycleLogFileNum: 0, KVNumOfLevels: 7,
				KVBlockSize: "32KiB", SaveBufferSize: "32KiB", MaxSaveBufferSize: "64MiB",
			},
		},
	},
	Storage: Storage{
		Engine: "pebble", VirtualShards: 256,
		Pebble: Pebble{Mode: "disk", DataDir: "./data", WALEnabled: true, CacheSize: "16MiB", MemtableSize: "64MiB", CompactionConcurrency: 1},
		Bolt:   Bolt{File: "./data/futureq.db", Bucket: "futureq"},
	},
	Publish: Publish{MinAckLevel: Quorum, ProposalTimeout: 5 * time.Second},
	Delivery: Delivery{
		ShardDistribution:          metadata.ShardDistributionConsumerCount,
		HeapType:                   HeapBinary,
		Subscriptions:              Subscriptions{RenewalWorkers: 4, DisconnectWorkers: 1},
		ConsumerRemovalWorkers:     1,
		DiscoveryWorkers:           2,
		ShardWorkers:               1,
		PreloadScanIdleMaxInterval: 0 * time.Millisecond,
		ConsumerQueueSize:          1024,
		TimeBucket:                 time.Millisecond,
		DispatchPollInterval:       5 * time.Millisecond,
		PreloadLookahead:           0,
		PreloadMaxMessages:         512,
		InFlightTimeout:            5 * time.Second,
		ReadPermit:                 time.Second,
		DeleteBatchInterval:        500 * time.Millisecond,
		DeleteWorkers:              16,
		AckQueueCapacity:           8192,
		ReceiptCache:               ReceiptCache{MaxBytes: "64MiB", MaxEntries: 1 << 19},
		TTLSweepInterval:           time.Minute,
		PrepareBatchSize:           64,
		PrepareBatchBytes:          "256KiB",
		ScanMaxKeys:                256,
		ScanWorkBudget:             5 * time.Millisecond,
		ValidationTimeout:          250 * time.Millisecond,
		DeliveryWorkers:            2,
	},
	Observability: Observability{Logging: Logger{Level: "info"}, Metrics: Metrics{Listen: "0.0.0.0:9090"}},
}

// DefaultDelivery returns a copy of the built-in delivery settings for direct
// constructors. Application startup overrides these with the loaded config.
func DefaultDelivery() Delivery { return defaultConfig.Delivery }
