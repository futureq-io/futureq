package config

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/futureq-io/futureq/pkg/raft/metadata"
	"github.com/futureq-io/futureq/pkg/utils"
	raftconfig "github.com/futureq-io/stormoar/v4/config"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v2"
)

type AckLevel = string

const (
	Quorum AckLevel = "quorum"
	Leader AckLevel = "leader"
	NoAck  AckLevel = "noAck"
)

// HeapType selects the node-local delivery candidate heap at startup.
type HeapType string

const (
	HeapBinary HeapType = "binary"
	Heap4Ary   HeapType = "4ary"
	Heap8Ary   HeapType = "8ary"
)

type Subscriptions struct {
	// AllowAutoAck permits workers to complete delivery on send instead of manual ACK/NACK.
	AllowAutoAck bool `mapstructure:"allowAutoAck" yaml:"allowAutoAck"`
	// DisconnectTimeout is the time without any connected worker before unsubscribe. Zero retains interest forever.
	DisconnectTimeout time.Duration `mapstructure:"disconnectTimeout" yaml:"disconnectTimeout"`
	RenewalWorkers    int           `mapstructure:"renewalWorkers" yaml:"renewalWorkers"`
	DisconnectWorkers int           `mapstructure:"disconnectWorkers" yaml:"disconnectWorkers"`
}

type Config struct {
	ConfigVersion int           `mapstructure:"configVersion" yaml:"configVersion"`
	API           API           `mapstructure:"api" yaml:"api"`
	Cluster       Cluster       `mapstructure:"cluster" yaml:"cluster"`
	Storage       Storage       `mapstructure:"storage" yaml:"storage"`
	Publish       Publish       `mapstructure:"publish" yaml:"publish"`
	Delivery      Delivery      `mapstructure:"delivery" yaml:"delivery"`
	Observability Observability `mapstructure:"observability" yaml:"observability"`
}

type API struct {
	GRPC GRPC `mapstructure:"grpc" yaml:"grpc"`
}

type GRPC struct {
	Listen                string        `mapstructure:"listen" yaml:"listen"`
	Advertise             string        `mapstructure:"advertise" yaml:"advertise"`
	MaxConcurrentStreams  uint32        `mapstructure:"maxConcurrentStreams" yaml:"maxConcurrentStreams"`
	MaxReceiveMessageSize Size          `mapstructure:"maxReceiveMessageSize" yaml:"maxReceiveMessageSize"`
	MaxSendMessageSize    Size          `mapstructure:"maxSendMessageSize" yaml:"maxSendMessageSize"`
	KeepaliveTimeout      time.Duration `mapstructure:"keepaliveTimeout" yaml:"keepaliveTimeout"`
	MaxConnectionIdle     time.Duration `mapstructure:"maxConnectionIdle" yaml:"maxConnectionIdle"`
	MaxConnectionAge      time.Duration `mapstructure:"maxConnectionAge" yaml:"maxConnectionAge"`
	MaxConnectionAgeGrace time.Duration `mapstructure:"maxConnectionAgeGrace" yaml:"maxConnectionAgeGrace"`
}

type Cluster struct {
	Enabled   bool     `mapstructure:"enabled" yaml:"enabled"`
	NodeID    uint64   `mapstructure:"nodeId" yaml:"nodeId"`
	ShardID   uint64   `mapstructure:"shardId" yaml:"shardId"`
	JoinSeeds []string `mapstructure:"joinSeeds" yaml:"joinSeeds"`
	Raft      Raft     `mapstructure:"raft" yaml:"raft"`
}

type Raft struct {
	Listen             string            `mapstructure:"listen" yaml:"listen"`
	Advertise          string            `mapstructure:"advertise" yaml:"advertise"`
	DataDir            string            `mapstructure:"dataDir" yaml:"dataDir"`
	InitialMembers     map[uint64]string `mapstructure:"initialMembers" yaml:"initialMembers"`
	RTT                time.Duration     `mapstructure:"rtt" yaml:"rtt"`
	SnapshotEntries    uint64            `mapstructure:"snapshotEntries" yaml:"snapshotEntries"`
	CompactionOverhead uint64            `mapstructure:"compactionOverhead" yaml:"compactionOverhead"`
	LogDB              LogDB             `mapstructure:"logDB" yaml:"logDB"`
}

// LogDB exposes every field of Dragonboat's LogDBConfig. Workers separately
// controls Engine.ExecShards, whose workers call SaveRaftState. The active Tan
// backend only consumes KVWriteBufferSize directly and retains 16 buffers.
type LogDB struct {
	Workers                            uint64 `mapstructure:"workers" yaml:"workers"`
	Shards                             uint64 `mapstructure:"shards" yaml:"shards"`
	KVKeepLogFileNum                   uint64 `mapstructure:"kvKeepLogFileNum" yaml:"kvKeepLogFileNum"`
	KVMaxBackgroundCompactions         uint64 `mapstructure:"kvMaxBackgroundCompactions" yaml:"kvMaxBackgroundCompactions"`
	KVMaxBackgroundFlushes             uint64 `mapstructure:"kvMaxBackgroundFlushes" yaml:"kvMaxBackgroundFlushes"`
	KVLRUCacheSize                     Size   `mapstructure:"kvLRUCacheSize" yaml:"kvLRUCacheSize"`
	KVWriteBufferSize                  Size   `mapstructure:"kvWriteBufferSize" yaml:"kvWriteBufferSize"`
	KVMaxWriteBufferNumber             uint64 `mapstructure:"kvMaxWriteBufferNumber" yaml:"kvMaxWriteBufferNumber"`
	KVLevel0FileNumCompactionTrigger   uint64 `mapstructure:"kvLevel0FileNumCompactionTrigger" yaml:"kvLevel0FileNumCompactionTrigger"`
	KVLevel0SlowdownWritesTrigger      uint64 `mapstructure:"kvLevel0SlowdownWritesTrigger" yaml:"kvLevel0SlowdownWritesTrigger"`
	KVLevel0StopWritesTrigger          uint64 `mapstructure:"kvLevel0StopWritesTrigger" yaml:"kvLevel0StopWritesTrigger"`
	KVMaxBytesForLevelBase             Size   `mapstructure:"kvMaxBytesForLevelBase" yaml:"kvMaxBytesForLevelBase"`
	KVMaxBytesForLevelMultiplier       uint64 `mapstructure:"kvMaxBytesForLevelMultiplier" yaml:"kvMaxBytesForLevelMultiplier"`
	KVTargetFileSizeBase               Size   `mapstructure:"kvTargetFileSizeBase" yaml:"kvTargetFileSizeBase"`
	KVTargetFileSizeMultiplier         uint64 `mapstructure:"kvTargetFileSizeMultiplier" yaml:"kvTargetFileSizeMultiplier"`
	KVLevelCompactionDynamicLevelBytes uint64 `mapstructure:"kvLevelCompactionDynamicLevelBytes" yaml:"kvLevelCompactionDynamicLevelBytes"`
	KVRecycleLogFileNum                uint64 `mapstructure:"kvRecycleLogFileNum" yaml:"kvRecycleLogFileNum"`
	KVNumOfLevels                      uint64 `mapstructure:"kvNumOfLevels" yaml:"kvNumOfLevels"`
	KVBlockSize                        Size   `mapstructure:"kvBlockSize" yaml:"kvBlockSize"`
	SaveBufferSize                     Size   `mapstructure:"saveBufferSize" yaml:"saveBufferSize"`
	MaxSaveBufferSize                  Size   `mapstructure:"maxSaveBufferSize" yaml:"maxSaveBufferSize"`
}

// DragonboatConfig converts byte quantities while preserving zero for options
// where it disables a cache/limit. Worker/shard counts are stored in Raft's
// compatibility flags and cannot be changed against existing Raft storage.
func (c LogDB) DragonboatConfig() (raftconfig.LogDBConfig, error) {
	maxInt := uint64(int(^uint(0) >> 1))
	if c.Workers == 0 || c.Shards == 0 || c.Workers > maxInt || c.Shards > maxInt {
		return raftconfig.LogDBConfig{}, fmt.Errorf("cluster.raft.logDB.workers and shards must be positive supported counts")
	}
	result := raftconfig.LogDBConfig{
		Shards: c.Shards, KVKeepLogFileNum: c.KVKeepLogFileNum,
		KVMaxBackgroundCompactions: c.KVMaxBackgroundCompactions, KVMaxBackgroundFlushes: c.KVMaxBackgroundFlushes,
		KVMaxWriteBufferNumber:           c.KVMaxWriteBufferNumber,
		KVLevel0FileNumCompactionTrigger: c.KVLevel0FileNumCompactionTrigger,
		KVLevel0SlowdownWritesTrigger:    c.KVLevel0SlowdownWritesTrigger, KVLevel0StopWritesTrigger: c.KVLevel0StopWritesTrigger,
		KVMaxBytesForLevelMultiplier: c.KVMaxBytesForLevelMultiplier, KVTargetFileSizeMultiplier: c.KVTargetFileSizeMultiplier,
		KVLevelCompactionDynamicLevelBytes: c.KVLevelCompactionDynamicLevelBytes,
		KVRecycleLogFileNum:                c.KVRecycleLogFileNum, KVNumOfLevels: c.KVNumOfLevels,
	}
	for _, field := range []struct {
		name     string
		size     Size
		target   *uint64
		positive bool
	}{
		{"kvLRUCacheSize", c.KVLRUCacheSize, &result.KVLRUCacheSize, false},
		{"kvWriteBufferSize", c.KVWriteBufferSize, &result.KVWriteBufferSize, true},
		{"kvMaxBytesForLevelBase", c.KVMaxBytesForLevelBase, &result.KVMaxBytesForLevelBase, false},
		{"kvTargetFileSizeBase", c.KVTargetFileSizeBase, &result.KVTargetFileSizeBase, false},
		{"kvBlockSize", c.KVBlockSize, &result.KVBlockSize, false},
		{"saveBufferSize", c.SaveBufferSize, &result.SaveBufferSize, false},
		{"maxSaveBufferSize", c.MaxSaveBufferSize, &result.MaxSaveBufferSize, false},
	} {
		var n int64
		var err error
		switch field.size {
		case "0B", "0KiB", "0MiB", "0GiB":
		default:
			n, err = field.size.Bytes()
		}
		if err != nil || uint64(n) > maxInt || (field.positive && n == 0) {
			return raftconfig.LogDBConfig{}, fmt.Errorf("cluster.raft.logDB.%s must be a supported size (kvWriteBufferSize must be positive)", field.name)
		}
		*field.target = uint64(n)
	}
	return result, nil
}

type Storage struct {
	Engine        string `mapstructure:"engine" yaml:"engine"`
	VirtualShards int    `mapstructure:"virtualShards" yaml:"virtualShards"`
	Pebble        Pebble `mapstructure:"pebble" yaml:"pebble"`
	Bolt          Bolt   `mapstructure:"bolt" yaml:"bolt"`
}

type Pebble struct {
	Mode                  string `mapstructure:"mode" yaml:"mode"`
	DataDir               string `mapstructure:"dataDir" yaml:"dataDir"`
	WALEnabled            bool   `mapstructure:"walEnabled" yaml:"walEnabled"`
	CacheSize             Size   `mapstructure:"cacheSize" yaml:"cacheSize"`
	MemtableSize          Size   `mapstructure:"memtableSize" yaml:"memtableSize"`
	CompactionConcurrency int    `mapstructure:"compactionConcurrency" yaml:"compactionConcurrency"`
}

type Bolt struct {
	File   string `mapstructure:"file" yaml:"file"`
	Bucket string `mapstructure:"bucket" yaml:"bucket"`
}

type Publish struct {
	MinAckLevel      AckLevel      `mapstructure:"minAckLevel" yaml:"minAckLevel"`
	ProposalTimeout  time.Duration `mapstructure:"proposalTimeout" yaml:"proposalTimeout"`
	BatchMaxRequests int           `mapstructure:"batchMaxRequests" yaml:"batchMaxRequests"`
	BatchMaxBytes    Size          `mapstructure:"batchMaxBytes" yaml:"batchMaxBytes"`
	BatchMaxWait     time.Duration `mapstructure:"batchMaxWait" yaml:"batchMaxWait"`
}

type ReceiptCache struct {
	MaxBytes   Size `mapstructure:"maxBytes" yaml:"maxBytes"`
	MaxEntries int  `mapstructure:"maxEntries" yaml:"maxEntries"`
}

// Bytes returns the decoded receipt budget. A zero budget disables the cache.
func (c ReceiptCache) Bytes() (int64, error) {
	if c.MaxBytes == "0B" {
		return 0, nil
	}
	return c.MaxBytes.Bytes()
}

const (
	MaxPublishBatchRequests = 256
	MaxPublishBatchBytes    = 16 << 20
)

type Delivery struct {
	ShardDistribution metadata.ShardDistribution `mapstructure:"shardDistribution" yaml:"shardDistribution"`

	Subscriptions              Subscriptions `mapstructure:"subscriptions" yaml:"subscriptions"`
	HeapType                   HeapType      `mapstructure:"heapType" yaml:"heapType"`
	DiscoveryWorkers           int           `mapstructure:"discoveryWorkers" yaml:"discoveryWorkers"`
	ShardWorkers               int           `mapstructure:"shardWorkers" yaml:"shardWorkers"`
	PreloadScanIdleMaxInterval time.Duration `mapstructure:"preloadScanIdleMaxInterval" yaml:"preloadScanIdleMaxInterval"`
	ConsumerQueueSize          int           `mapstructure:"consumerQueueSize" yaml:"consumerQueueSize"`
	TimeBucket                 time.Duration `mapstructure:"timeBucket" yaml:"timeBucket"`
	DispatchPollInterval       time.Duration `mapstructure:"dispatchPollInterval" yaml:"dispatchPollInterval"`
	PreloadLookahead           time.Duration `mapstructure:"preloadLookahead" yaml:"preloadLookahead"`
	PreloadMaxMessages         int           `mapstructure:"preloadMaxMessages" yaml:"preloadMaxMessages"`
	InFlightTimeout            time.Duration `mapstructure:"inFlightTimeout" yaml:"inFlightTimeout"`
	ReadPermit                 time.Duration `mapstructure:"readPermit" yaml:"readPermit"`
	DeleteBatchInterval        time.Duration `mapstructure:"deleteBatchInterval" yaml:"deleteBatchInterval"`
	DeleteWorkers              int           `mapstructure:"deleteWorkers" yaml:"deleteWorkers"`
	ReceiptCache               ReceiptCache  `mapstructure:"receiptCache" yaml:"receiptCache"`
	AckQueueCapacity           int           `mapstructure:"ackQueueCapacity" yaml:"ackQueueCapacity"`
	ConsumerRemovalWorkers     int           `mapstructure:"consumerRemovalWorkers" yaml:"consumerRemovalWorkers"`
	TTLSweepInterval           time.Duration `mapstructure:"ttlSweepInterval" yaml:"ttlSweepInterval"`
	PrepareBatchSize           int           `mapstructure:"prepareBatchSize" yaml:"prepareBatchSize"`
	PrepareBatchBytes          Size          `mapstructure:"prepareBatchBytes" yaml:"prepareBatchBytes"`
	ScanMaxKeys                int           `mapstructure:"scanMaxKeys" yaml:"scanMaxKeys"`
	ScanWorkBudget             time.Duration `mapstructure:"scanWorkBudget" yaml:"scanWorkBudget"`
	ValidationTimeout          time.Duration `mapstructure:"validationTimeout" yaml:"validationTimeout"`
	DeliveryWorkers            int           `mapstructure:"deliveryWorkers" yaml:"deliveryWorkers"`
}

type Observability struct {
	Logging Logger  `mapstructure:"logging" yaml:"logging"`
	Metrics Metrics `mapstructure:"metrics" yaml:"metrics"`
}

type Metrics struct {
	Listen string `mapstructure:"listen" yaml:"listen"`
}
type Logger struct {
	Level string `mapstructure:"level" yaml:"level"`
}

// Size is a binary byte quantity such as "100KiB" or "16MiB".
type Size string

func (s Size) Bytes() (int64, error) {
	value := string(s)
	for _, unit := range []struct {
		suffix string
		factor int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1},
	} {
		if number, ok := strings.CutSuffix(value, unit.suffix); ok {
			n, err := strconv.ParseInt(number, 10, 64)
			if err != nil || n <= 0 || n > (int64(^uint64(0)>>1))/unit.factor {
				return 0, fmt.Errorf("invalid size %q", value)
			}
			return n * unit.factor, nil
		}
	}
	return 0, fmt.Errorf("invalid size %q (use B, KiB, MiB, or GiB)", value)
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")
	v.SetEnvPrefix("futureq")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AllowEmptyEnv(true)
	v.AutomaticEnv()

	defaultBytes, err := yaml.Marshal(defaultConfig)
	if err != nil {
		return nil, fmt.Errorf("marshal default config: %w", err)
	}
	if err := v.ReadConfig(bytes.NewReader(defaultBytes)); err != nil {
		return nil, fmt.Errorf("read default config: %w", err)
	}
	if path != "" {
		v.SetConfigFile(path)
		if err := v.MergeInConfig(); err != nil {
			return nil, fmt.Errorf("merge config: %w", err)
		}
	}
	if err := bindEnv(v, reflect.TypeOf(Config{}), ""); err != nil {
		return nil, err
	}
	// Viper cannot decode a map from one environment string. This value replaces
	// the entire initialMembers map and uses YAML syntax, e.g. '{1: "host:50005"}'.
	if raw, ok := os.LookupEnv("FUTUREQ_CLUSTER_RAFT_INITIALMEMBERS"); ok {
		members := map[uint64]string{}
		if err := yaml.Unmarshal([]byte(raw), &members); err != nil {
			return nil, fmt.Errorf("FUTUREQ_CLUSTER_RAFT_INITIALMEMBERS: %w", err)
		}
		v.Set("cluster.raft.initialMembers", members)
	}
	var c Config
	if err := v.UnmarshalExact(&c); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &c, nil
}

// Bind every documented leaf so env-only values participate in UnmarshalExact.
func bindEnv(v *viper.Viper, typ reflect.Type, prefix string) error {
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		key := field.Tag.Get("mapstructure")
		if key == "" {
			continue
		}
		if prefix != "" {
			key = prefix + "." + key
		}
		if field.Type.Kind() == reflect.Struct {
			if err := bindEnv(v, field.Type, key); err != nil {
				return err
			}
			continue
		}
		if err := v.BindEnv(key); err != nil {
			return fmt.Errorf("bind environment for %s: %w", key, err)
		}
	}
	return nil
}

func (c *Config) validate() error {
	if c.ConfigVersion != 1 {
		return fmt.Errorf("configVersion must be 1")
	}
	if err := c.validateAPI(); err != nil {
		return err
	}
	if c.Observability.Metrics.Listen != "" && !validAddress(c.Observability.Metrics.Listen, false) {
		return fmt.Errorf("observability.metrics.listen must be empty or a host:port address")
	}
	if err := c.validateStorage(); err != nil {
		return err
	}
	if err := c.validateCluster(); err != nil {
		return err
	}
	if err := c.validatePublishAndDelivery(); err != nil {
		return err
	}
	return nil
}

func validAddress(address string, advertised bool) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return false
	}
	return !advertised || (host != "0.0.0.0" && host != "::")
}

func (c *Config) validateAPI() error {
	g := c.API.GRPC
	if !validAddress(g.Listen, false) {
		return fmt.Errorf("api.grpc.listen must be a host:port address")
	}
	if g.Advertise != "" && !validAddress(g.Advertise, true) {
		return fmt.Errorf("api.grpc.advertise must be a reachable host:port address")
	}
	if g.MaxConcurrentStreams == 0 || g.KeepaliveTimeout <= 0 {
		return fmt.Errorf("api.grpc.maxConcurrentStreams and keepaliveTimeout must be positive")
	}
	for name, size := range map[string]Size{"maxReceiveMessageSize": g.MaxReceiveMessageSize, "maxSendMessageSize": g.MaxSendMessageSize} {
		bytes, err := size.Bytes()
		if err != nil || bytes > int64(int(^uint(0)>>1)) {
			return fmt.Errorf("api.grpc.%s must be a positive supported size: %v", name, err)
		}
	}
	return nil
}

func (c *Config) validateCluster() error {
	if !c.Cluster.Enabled {
		return nil
	}
	cl := c.Cluster
	if cl.NodeID == 0 || cl.ShardID == 0 {
		return fmt.Errorf("cluster.nodeId and cluster.shardId must be nonzero")
	}
	if !validAddress(c.API.GRPC.Advertise, true) {
		return fmt.Errorf("api.grpc.advertise is required when cluster.enabled is true")
	}
	if !validAddress(cl.Raft.Listen, false) || !validAddress(cl.Raft.Advertise, true) {
		return fmt.Errorf("cluster.raft.listen and advertise must be valid host:port addresses")
	}
	if cl.Raft.DataDir == "" || cl.Raft.RTT < time.Millisecond || cl.Raft.RTT%time.Millisecond != 0 || cl.Raft.SnapshotEntries == 0 || cl.Raft.CompactionOverhead == 0 {
		return fmt.Errorf("cluster.raft requires dataDir, millisecond rtt, snapshotEntries, and compactionOverhead")
	}
	if _, err := cl.Raft.LogDB.DragonboatConfig(); err != nil {
		return err
	}
	if len(cl.JoinSeeds) > 0 && len(cl.Raft.InitialMembers) > 0 {
		return fmt.Errorf("cluster.joinSeeds and cluster.raft.initialMembers cannot both be set")
	}
	for _, seed := range cl.JoinSeeds {
		if !validAddress(seed, true) {
			return fmt.Errorf("cluster.joinSeeds contains invalid address %q", seed)
		}
	}
	for id, addr := range cl.Raft.InitialMembers {
		if id == 0 || !validAddress(addr, true) {
			return fmt.Errorf("cluster.raft.initialMembers contains invalid member %d: %q", id, addr)
		}
	}
	if len(cl.Raft.InitialMembers) > 0 && cl.Raft.InitialMembers[cl.NodeID] != cl.Raft.Advertise {
		return fmt.Errorf("cluster.raft.initialMembers must include this nodeId with its raft advertise address")
	}
	return nil
}

func (c *Config) validateStorage() error {
	if !utils.ValidVirtualShardCount(c.Storage.VirtualShards) {
		return fmt.Errorf("storage.virtualShards must be a power of two from 1 through %d", utils.MaxVirtualShards)
	}
	switch c.Storage.Engine {
	case "pebble":
		p := c.Storage.Pebble
		if p.CompactionConcurrency < 1 {
			return fmt.Errorf("storage.pebble.compactionConcurrency must be at least 1")
		}
		if p.Mode != "disk" && p.Mode != "memory" {
			return fmt.Errorf("storage.pebble.mode must be disk or memory")
		}
		if p.Mode == "disk" && p.DataDir == "" {
			return fmt.Errorf("storage.pebble.dataDir is required in disk mode")
		}
		if !p.WALEnabled && !c.Cluster.Enabled && p.Mode == "disk" {
			return fmt.Errorf("storage.pebble.walEnabled cannot be false for a standalone disk node")
		}
		for name, size := range map[string]Size{"cacheSize": p.CacheSize, "memtableSize": p.MemtableSize} {
			if _, err := size.Bytes(); err != nil {
				return fmt.Errorf("storage.pebble.%s: %w", name, err)
			}
		}
	case "bolt":
		if c.Storage.Bolt.File == "" {
			return fmt.Errorf("storage.bolt.file is required")
		}
	default:
		return fmt.Errorf("storage.engine must be pebble or bolt")
	}
	return nil
}

func (c *Config) validatePublishAndDelivery() error {
	if !c.Delivery.ShardDistribution.Valid() {
		return fmt.Errorf("delivery.shardDistribution must be consumerCount or replicaCount")
	}
	if c.Delivery.Subscriptions.DisconnectTimeout < 0 || c.Delivery.Subscriptions.DisconnectTimeout%time.Millisecond != 0 {
		return fmt.Errorf("delivery.subscriptions.disconnectTimeout must be zero (forever) or a positive whole number of milliseconds")
	}
	for _, setting := range []struct {
		key   string
		value int
	}{
		{"delivery.subscriptions.renewalWorkers", c.Delivery.Subscriptions.RenewalWorkers},
		{"delivery.subscriptions.disconnectWorkers", c.Delivery.Subscriptions.DisconnectWorkers},
		{"delivery.consumerRemovalWorkers", c.Delivery.ConsumerRemovalWorkers},
	} {
		if setting.value < 1 || setting.value > 16 {
			return fmt.Errorf("%s must be 1..16", setting.key)
		}
	}
	switch c.Publish.MinAckLevel {
	case Quorum, Leader, NoAck:
	default:
		return fmt.Errorf("publish.minAckLevel must be quorum, leader, or noAck")
	}
	if c.Publish.ProposalTimeout <= 0 {
		return fmt.Errorf("publish.proposalTimeout must be positive")
	}
	if c.Publish.BatchMaxRequests < 1 || c.Publish.BatchMaxRequests > MaxPublishBatchRequests {
		return fmt.Errorf("publish.batchMaxRequests must be 1..%d", MaxPublishBatchRequests)
	}
	if size, err := c.Publish.BatchMaxBytes.Bytes(); err != nil || size > MaxPublishBatchBytes {
		return fmt.Errorf("publish.batchMaxBytes must be a positive size up to 16MiB")
	}
	if c.Publish.BatchMaxWait <= 0 || c.Publish.BatchMaxWait > time.Second {
		return fmt.Errorf("publish.batchMaxWait must be in (0, 1s]")
	}

	d := c.Delivery
	switch d.HeapType {
	case HeapBinary, Heap4Ary, Heap8Ary:
	default:
		return fmt.Errorf("delivery.heapType must be binary, 4ary, or 8ary")
	}
	if d.ReadPermit < 100*time.Millisecond || d.ReadPermit > 30*time.Second || d.ReadPermit%time.Millisecond != 0 {
		return fmt.Errorf("delivery.readPermit must be whole milliseconds from 100ms to 30s")
	}
	if d.DiscoveryWorkers < 1 || d.DiscoveryWorkers > 16 {
		return fmt.Errorf("delivery.discoveryWorkers must be 1..16")
	}
	if d.ShardWorkers < 1 {
		return fmt.Errorf("delivery.shardWorkers must be positive")
	}
	if d.PreloadScanIdleMaxInterval < 0 {
		return fmt.Errorf("delivery.preloadScanIdleMaxInterval must be nonnegative")
	}
	if d.PreloadLookahead < 0 || d.PreloadLookahead%time.Millisecond != 0 {
		return fmt.Errorf("delivery.preloadLookahead must be zero or a positive whole number of milliseconds")
	}
	if d.PreloadMaxMessages < 0 || d.PreloadMaxMessages > 1048576 {
		return fmt.Errorf("delivery.preloadMaxMessages must be 0..1048576")
	}
	if d.ConsumerQueueSize < 1 || d.ConsumerQueueSize > 65536 {
		return fmt.Errorf("delivery.consumerQueueSize must be 1..65536")
	}
	if d.TimeBucket < 0 || (d.TimeBucket > 0 && d.TimeBucket < time.Millisecond) {
		return fmt.Errorf("delivery.timeBucket must be zero or at least 1ms")
	}
	if d.DispatchPollInterval <= 0 || d.InFlightTimeout <= 0 || d.DeleteBatchInterval <= 0 || d.TTLSweepInterval <= 0 {
		return fmt.Errorf("delivery intervals and timeout must be positive")
	}
	if d.DeleteWorkers < 1 || d.DeleteWorkers > 256 {
		return fmt.Errorf("delivery.deleteWorkers must be 1..256")
	}
	if d.AckQueueCapacity < 1 || d.AckQueueCapacity > 1048576 {
		return fmt.Errorf("delivery.ackQueueCapacity must be 1..1048576")
	}
	if _, err := d.ReceiptCache.Bytes(); err != nil {
		return fmt.Errorf("delivery.receiptCache.maxBytes: %w", err)
	}
	const maxReceiptCacheEntries = 1 << 24
	entries := d.ReceiptCache.MaxEntries
	if entries < 1 || entries > maxReceiptCacheEntries || entries&(entries-1) != 0 {
		return fmt.Errorf("delivery.receiptCache.maxEntries must be a power of two from 1 through %d", maxReceiptCacheEntries)
	}
	if d.PrepareBatchSize < 1 || d.PrepareBatchSize > 256 || d.ScanMaxKeys < d.PrepareBatchSize || d.ScanMaxKeys > 65536 {
		return fmt.Errorf("delivery.prepareBatchSize must be 1..256 and scanMaxKeys must be between batch size and 65536")
	}
	if size, err := d.PrepareBatchBytes.Bytes(); err != nil || size < 1024 || size > 256<<10 {
		return fmt.Errorf("delivery.prepareBatchBytes must be 1KiB..256KiB")
	}
	if d.ScanWorkBudget <= 0 || d.ScanWorkBudget > 50*time.Millisecond || d.ValidationTimeout <= 0 || d.ValidationTimeout > time.Second || d.DeliveryWorkers < 1 || d.DeliveryWorkers > 16 {
		return fmt.Errorf("delivery requires scanWorkBudget in (0,50ms], validationTimeout in (0,1s], and deliveryWorkers in 1..16")
	}
	return nil
}
