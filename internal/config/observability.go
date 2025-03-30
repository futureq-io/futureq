package config

type Observability struct {
	Logging Logging `mapstructure:"logging" yaml:"logging"`
}

type Logging struct {
	Level string `mapstructure:"level" yaml:"level"`
}
