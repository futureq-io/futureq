package config

var defaultConfig = Config{
	Observability: Observability{
		Logging: Logging{
			Level: "info",
		},
	},
	RabbitMQ: nil,
}
