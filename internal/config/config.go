package config

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Observability Observability `mapstructure:"observability" yaml:"observability"`
	RabbitMQ      *RabbitMQ     `mapstructure:"rabbitmq" yaml:"rabbitmq"`
}

func PrepareConfig(path *string) (*Config, error) {
	var c Config

	v := viper.New()
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.SetEnvPrefix("futureq")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()

	defaultConfigBytes, err := yaml.Marshal(&defaultConfig)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling default config: %w", err)
	}

	err = v.ReadConfig(bytes.NewReader(defaultConfigBytes))
	if err != nil {
		return nil, fmt.Errorf("error reading default config: %w", err)
	}

	if path != nil && *path != "" {
		v.SetConfigFile(*path)
		err = v.MergeInConfig()
		if err != nil {
			return nil, fmt.Errorf("error merge config: %w", err)
		}
	}

	err = v.Unmarshal(&c)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling config: %w", err)
	}

	return &c, nil
}
