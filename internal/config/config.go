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

	"github.com/spf13/viper"
	"gopkg.in/yaml.v2"
)

type AckLevel = string

const (
	Quorum AckLevel = "quorum"
	Leader AckLevel = "leader"
	NoAck  AckLevel = "noAck"
)

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
}

type Storage struct {
	Engine string `mapstructure:"engine" yaml:"engine"`
	Pebble Pebble `mapstructure:"pebble" yaml:"pebble"`
	Bolt   Bolt   `mapstructure:"bolt" yaml:"bolt"`
}

type Pebble struct {
	Mode         string `mapstructure:"mode" yaml:"mode"`
	DataDir      string `mapstructure:"dataDir" yaml:"dataDir"`
	WALEnabled   bool   `mapstructure:"walEnabled" yaml:"walEnabled"`
	CacheSize    Size   `mapstructure:"cacheSize" yaml:"cacheSize"`
	MemtableSize Size   `mapstructure:"memtableSize" yaml:"memtableSize"`
}

type Bolt struct {
	File   string `mapstructure:"file" yaml:"file"`
	Bucket string `mapstructure:"bucket" yaml:"bucket"`
}

type Publish struct {
	MinAckLevel     AckLevel      `mapstructure:"minAckLevel" yaml:"minAckLevel"`
	ProposalTimeout time.Duration `mapstructure:"proposalTimeout" yaml:"proposalTimeout"`
}

type Delivery struct {
	TimeBucket           time.Duration `mapstructure:"timeBucket" yaml:"timeBucket"`
	DispatchPollInterval time.Duration `mapstructure:"dispatchPollInterval" yaml:"dispatchPollInterval"`
	InFlightTimeout      time.Duration `mapstructure:"inFlightTimeout" yaml:"inFlightTimeout"`
	DeleteBatchInterval  time.Duration `mapstructure:"deleteBatchInterval" yaml:"deleteBatchInterval"`
	TTLSweepInterval     time.Duration `mapstructure:"ttlSweepInterval" yaml:"ttlSweepInterval"`
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
	switch c.Storage.Engine {
	case "pebble":
		p := c.Storage.Pebble
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
	switch c.Publish.MinAckLevel {
	case Quorum, Leader, NoAck:
	default:
		return fmt.Errorf("publish.minAckLevel must be quorum, leader, or noAck")
	}
	if c.Publish.ProposalTimeout <= 0 {
		return fmt.Errorf("publish.proposalTimeout must be positive")
	}
	d := c.Delivery
	if d.TimeBucket < 0 || (d.TimeBucket > 0 && d.TimeBucket < time.Millisecond) {
		return fmt.Errorf("delivery.timeBucket must be zero or at least 1ms")
	}
	if d.DispatchPollInterval <= 0 || d.InFlightTimeout <= 0 || d.DeleteBatchInterval <= 0 || d.TTLSweepInterval <= 0 {
		return fmt.Errorf("delivery intervals and timeout must be positive")
	}
	return nil
}
