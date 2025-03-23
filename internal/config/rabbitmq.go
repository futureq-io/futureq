package config

import (
	"fmt"
)

type RabbitMQ struct {
	RabbitMQServer       RabbitMQServer       `mapstructure:"rabbitmq_server" yaml:"rabbitmq_server"`
	RabbitMQDataExchange RabbitMQDataExchange `mapstructure:"rabbitmq_data_exchange" yaml:"rabbitmq_data_exchange"`
}

type RabbitMQServer struct {
	Host        string `mapstructure:"host" yaml:"host"`
	Port        uint   `mapstructure:"port" yaml:"port"`
	Username    string `mapstructure:"username" yaml:"username"`
	Password    string `mapstructure:"password" yaml:"password"`
	VirtualHost string `mapstructure:"virtual_host" yaml:"virtual_host"`
}

type RabbitMQDataExchange struct {
	ConsumeQueueName string `mapstructure:"consume_queue_name" yaml:"consume_queue_name"`
	DeclareQueue     bool   `mapstructure:"declare_queue" yaml:"declare_queue"`
	ProduceQueueName string `mapstructure:"produce_queue_name" yaml:"produce_queue_name"`
}

func (r RabbitMQServer) ConnectionURI() string {
	return fmt.Sprintf("amqp://%s:%s@%s:%d/%s", r.Username, r.Password, r.Host, r.Port, r.VirtualHost)
}
