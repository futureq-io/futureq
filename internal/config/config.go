package config

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v2"
)

type AckLevel = string

const (
	Quorum AckLevel = "Quorum"
	NoAck  AckLevel = "NoAck"
)

type Config struct {
	Server        Server        `mapstructure:"server" yaml:"server"`
	Observability Observability `mapstructure:"observability" yaml:"observability"`
	Storage       Storage       `mapstructure:"storage" yaml:"storage"`
	Raft          Raft          `mapstructure:"raft" yaml:"raft"`
	Consumer      Consumer      `mapstructure:"consumer" yaml:"consumer"`
}

type Server struct {
	Listen        string        `mapstructure:"listen" yaml:"listen"`
	MaxConns      uint32        `mapstructure:"maxConns" yaml:"maxConns"`
	Timeout       time.Duration `mapstructure:"timeout" yaml:"timeout"`
	MaxRecvSizeKB int           `mapstructure:"maxRecvSizeKb" yaml:"maxRecvSizeKb"`
	MaxSendSizeKB int           `mapstructure:"maxSendSizeKb" yaml:"maxSendSizeKb"`
}

type Observability struct {
	Logger  Logger  `mapstructure:"logger" yaml:"logger"`
	Metrics Metrics `mapstructure:"metrics" yaml:"metrics"`
}

type Metrics struct {
	Addr string `mapstructure:"addr" yaml:"addr"`
}

type Logger struct {
	Level string `mapstructure:"level" yaml:"level"`
}

type Storage struct {
	MinAckLevel    AckLevel      `mapstructure:"minAckLevel" yaml:"minAckLevel"`
	Persist        bool          `mapstructure:"persist" yaml:"persist"`
	TimeBucketSize time.Duration `mapstructure:"timeBucketSize" yaml:"timeBucketSize"`
	Type           string        `mapstructure:"type" yaml:"type"`
	Pebble         Pebble        `mapstructure:"pebble" yaml:"pebble"`
	Bolt           Bolt
}

type Bolt struct {
	DataPath      string `mapstructure:"dataPath" yaml:"dataPath"`
	DefaultBucket string `mapstructure:"defaultBucket" yaml:"defaultBucket"`
}

type Pebble struct {
	DisableWAL       bool   `mapstructure:"disableWAL" yaml:"disableWAL"`
	DataPath         string `mapstructure:"dataPath" yaml:"dataPath"`
	CacheSizeMB      int64  `mapstructure:"cacheSizeMb" yaml:"cacheSizeMb"`
	InMemTableSizeMB uint64 `mapstructure:"inMemoryTableSizeMb" yaml:"inMemoryTableSizeMb"`
}

type Raft struct {
	Enabled        bool              `mapstructure:"enabled" yaml:"enabled"`
	NodeID         uint64            `mapstructure:"nodeId" yaml:"nodeId"`
	ClusterID      uint64            `mapstructure:"clusterId" yaml:"clusterId"`
	ListenAddress  string            `mapstructure:"listenAddress" yaml:"listenAddress"`
	DataPath       string            `mapstructure:"dataPath" yaml:"dataPath"`
	InitialMembers map[uint64]string `mapstructure:"initialMembers" yaml:"initialMembers"`

	// RTTMillisecond is the average round-trip latency between Raft peers in
	// milliseconds.  Dragonboat uses this to calibrate election timeouts and
	// heartbeat intervals.  Lower values mean faster leader failover but
	// higher network overhead.  Default: 200.
	RTTMillisecond uint64 `mapstructure:"rttMillisecond" yaml:"rttMillisecond"`

	SnapshotEntries    uint64 `mapstructure:"snapShotEntries" yaml:"snapShotEntries"`
	CompactionOverhead uint64 `mapstructure:"compactionOverHead" yaml:"compactionOverHead"`
}

// Consumer holds configuration for the message dispatch subsystem.
type Consumer struct {
	// DispatchPollIntervalMs is how long the dispatcher sleeps between scan
	// passes when no ready messages were found. Shorter values reduce delivery
	// latency at the cost of more Pebble iterator overhead. Default: 50ms.
	DispatchPollIntervalMs uint64 `mapstructure:"dispatchPollIntervalMs" yaml:"dispatchPollIntervalMs"`

	// DeleteBatchIntervalMs is how often the batched deleter flushes accumulated
	// acknowledged-message keys via Raft. Default: 500ms.
	DeleteBatchIntervalMs uint64 `mapstructure:"deleteBatchIntervalMs" yaml:"deleteBatchIntervalMs"`

	// InFlightTimeoutMs is the duration after which a dispatched-but-unacknowledged
	// message is considered abandoned and eligible for re-dispatch. Default: 5000ms.
	InFlightTimeoutMs uint64 `mapstructure:"inFlightTimeoutMs" yaml:"inFlightTimeoutMs"`

	// TTLJanitorIntervalMs is how often the TTL janitor performs a full Pebble
	// scan to remove expired messages that were never consumed. Default: 60000ms.
	TTLJanitorIntervalMs uint64 `mapstructure:"ttlJanitorIntervalMs" yaml:"ttlJanitorIntervalMs"`
}

func Load(path string) (*Config, error) {
	var c Config

	v := viper.New()
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.SetEnvPrefix("futureq")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()

	defaultConfigBytes, err := yaml.Marshal(defaultConfig)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling default config: %w", err)
	}

	err = v.ReadConfig(bytes.NewReader(defaultConfigBytes))
	if err != nil {
		return nil, fmt.Errorf("error reading default config: %w", err)
	}

	if path != "" {
		v.SetConfigFile(path)
		err = v.MergeInConfig()
		if err != nil {
			return nil, fmt.Errorf("error merge config: %w", err)
		}
	}

	err = v.Unmarshal(&c)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling config: %w", err)
	}

	if err := c.runPostLoadHooks(); err != nil {
		return nil, fmt.Errorf("failed to run post load hooks for config: %w", err)
	}

	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("error validating config: %w", err)
	}

	return &c, nil
}

func (c *Config) validate() error {
	if err := c.validateStorage(); err != nil {
		return err
	}

	return nil
}

func (c *Config) validateRaft() error {
	if c.Raft.SnapshotEntries == 0 {
		return fmt.Errorf("raft's snapshot entries cannot be zero")
	}

	if c.Raft.CompactionOverhead == 0 {
		return fmt.Errorf("raft's compaction overhead cannot be zero")
	}

	if c.Raft.RTTMillisecond == 0 {
		return fmt.Errorf("raft's rtt millisecond cannot be zero")
	}

	return nil
}

func (c *Config) validateStorage() error {
	if c.Storage.Persist && c.Storage.Pebble.DataPath == "" {
		return fmt.Errorf("pebble's data path cannot be empty when persist is true")
	}

	if c.Storage.TimeBucketSize < 1*time.Millisecond {
		c.Storage.TimeBucketSize = 0
	}

	if !c.Raft.Enabled {
		if c.Storage.Pebble.DisableWAL {
			return fmt.Errorf("WAL can not be disabled on single node setup without raft")
		}
	}

	if c.Storage.Type != "pebble" && c.Storage.Type != "bolt" {
		return fmt.Errorf("storage type can only be in (pebble, bolt)")
	}

	return nil
}

func (c *Config) runPostLoadHooks() error {
	if !c.Storage.Persist {
		c.Storage.Pebble.DataPath = ""
		c.Storage.Bolt.DataPath = ""
	}

	return nil
}
