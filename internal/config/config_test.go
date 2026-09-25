package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"gopkg.in/yaml.v2"
)

type ConfigSuite struct {
	suite.Suite
}

func TestConfigSuite(t *testing.T) {
	suite.Run(t, new(ConfigSuite))
}

func (s *ConfigSuite) TestExampleConfigMatchesDefault() {
	require := s.Require()

	examplePath := filepath.Join("../..", "config.example.yaml")
	exampleBytes, err := os.ReadFile(examplePath)
	require.NoError(err)

	var exampleMap map[string]interface{}
	err = yaml.Unmarshal(exampleBytes, &exampleMap)
	require.NoError(err)

	defaultBytes, err := yaml.Marshal(defaultConfig)
	require.NoError(err)

	var defaultMap map[string]interface{}
	err = yaml.Unmarshal(defaultBytes, &defaultMap)
	require.NoError(err)

	require.Equal(defaultMap, exampleMap)

	fromDefaults, err := Load("")
	require.NoError(err)
	fromExample, err := Load(examplePath)
	require.NoError(err)
	require.Equal(fromDefaults, fromExample)
}

func (s *ConfigSuite) TestPartialFileAndEnvironmentOverrides() {
	path := s.writeConfig("api:\n  grpc:\n    listen: '127.0.0.1:8444'\ndelivery:\n  inFlightTimeout: 9s\n")
	s.T().Setenv("FUTUREQ_API_GRPC_LISTEN", "127.0.0.1:8445")
	s.T().Setenv("FUTUREQ_CLUSTER_NODEID", "7")
	s.T().Setenv("FUTUREQ_DELIVERY_INFLIGHTTIMEOUT", "11s")
	s.T().Setenv("FUTUREQ_STORAGE_PEBBLE_CACHESIZE", "32MiB")

	cfg, err := Load(path)
	s.Require().NoError(err)
	s.Equal("127.0.0.1:8445", cfg.API.GRPC.Listen)
	s.Equal(uint64(7), cfg.Cluster.NodeID)
	s.Equal(11*time.Second, cfg.Delivery.InFlightTimeout)
	s.Equal(Size("32MiB"), cfg.Storage.Pebble.CacheSize)
	s.Equal(time.Millisecond, cfg.Delivery.TimeBucket)
}

func (s *ConfigSuite) TestClusterBootstrapAndJoinSettings() {
	bootstrap := s.writeConfig("cluster:\n  enabled: true\n  nodeId: 1\n  raft:\n    advertise: 'node1.example:50005'\n    initialMembers:\n      1: 'node1.example:50005'\napi:\n  grpc:\n    advertise: 'node1.example:8443'\n")
	cfg, err := Load(bootstrap)
	s.Require().NoError(err)
	s.Equal("node1.example:50005", cfg.Cluster.Raft.InitialMembers[1])

	s.T().Setenv("FUTUREQ_CLUSTER_RAFT_INITIALMEMBERS", "{1: 'node1.example:50005'}")
	cfg, err = Load(bootstrap)
	s.Require().NoError(err)
	s.Equal("node1.example:50005", cfg.Cluster.Raft.InitialMembers[1])

	s.T().Setenv("FUTUREQ_CLUSTER_RAFT_INITIALMEMBERS", "{}")
	s.T().Setenv("FUTUREQ_CLUSTER_JOINSEEDS", "node2.example:8443,node3.example:8443")
	cfg, err = Load(bootstrap)
	s.Require().NoError(err)
	s.Empty(cfg.Cluster.Raft.InitialMembers)
	s.Equal([]string{"node2.example:8443", "node3.example:8443"}, cfg.Cluster.JoinSeeds)
}

func (s *ConfigSuite) TestEmptyEnvironmentValueDisablesMetrics() {
	s.T().Setenv("FUTUREQ_OBSERVABILITY_METRICS_LISTEN", "")
	cfg, err := Load("")
	s.Require().NoError(err)
	s.Empty(cfg.Observability.Metrics.Listen)
}

func (s *ConfigSuite) TestInvalidEnvironmentValueFails() {
	s.T().Setenv("FUTUREQ_CLUSTER_NODEID", "not-a-number")
	_, err := Load("")
	s.Require().ErrorContains(err, "nodeId")
}

func (s *ConfigSuite) TestRejectsUnknownAndInvalidValues() {
	tests := []struct{ name, yaml, want string }{
		{"old schema", "server:\n  listen: ':8443'\n", "server"},
		{"unknown nested key", "api:\n  grpc:\n    maxConns: 10\n", "maxconns"},
		{"version", "configVersion: 2\n", "configVersion"},
		{"size", "api:\n  grpc:\n    maxSendMessageSize: 0KiB\n", "maxSendMessageSize"},
		{"missing advertise", "cluster:\n  enabled: true\n", "api.grpc.advertise"},
		{"unspecified advertise", "cluster:\n  enabled: true\napi:\n  grpc:\n    advertise: '0.0.0.0:8443'\n", "api.grpc.advertise"},
		{"bootstrap and join", "cluster:\n  enabled: true\n  joinSeeds: ['node2.example:8443']\n  raft:\n    advertise: 'node1.example:50005'\n    initialMembers: {1: 'node1.example:50005'}\napi:\n  grpc:\n    advertise: 'node1.example:8443'\n", "cannot both be set"},
		{"missing bolt file", "storage:\n  engine: bolt\n  bolt:\n    file: ''\n", "storage.bolt.file"},
		{"zero dispatch interval", "delivery:\n  dispatchPollInterval: 0s\n", "delivery intervals"},
	}
	for _, tt := range tests {
		s.Run(tt.name, func() {
			_, err := Load(s.writeConfig(tt.yaml))
			s.Require().ErrorContains(err, tt.want)
		})
	}
}

func (s *ConfigSuite) TestMemoryPebbleAndBolt() {
	for _, document := range []string{
		"storage:\n  engine: pebble\n  pebble:\n    mode: memory\n    dataDir: ''\n",
		"storage:\n  engine: bolt\n",
	} {
		_, err := Load(s.writeConfig(document))
		s.Require().NoError(err)
	}
}

func (s *ConfigSuite) writeConfig(document string) string {
	path := filepath.Join(s.T().TempDir(), "config.yaml")
	s.Require().NoError(os.WriteFile(path, []byte(document), 0600))
	return path
}
