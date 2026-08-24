package config

import (
	"os"
	"path/filepath"
	"testing"

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
}
