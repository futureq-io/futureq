package q

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/storage"
)

type rabbitMQ struct {
	cfg        config.RabbitMQ
	logger     *zap.Logger
	rabbitConn *amqp.Connection
	rabbitChan *amqp.Channel
}

func NewRabbitMQ(cfg config.RabbitMQ, logger *zap.Logger) Q {
	return &rabbitMQ{
		cfg:    cfg,
		logger: logger,
	}
}

func (r *rabbitMQ) Connect() error {
	var err error

	r.rabbitConn, err = amqp.Dial(r.cfg.RabbitMQServer.ConnectionURI())
	if err != nil {
		return fmt.Errorf("error in connecting to rabbitmq: %w", err)
	}

	r.rabbitChan, err = r.rabbitConn.Channel()
	if err != nil {
		return fmt.Errorf("error in creating channel to rabbitmq: %w", err)
	}

	return nil
}

func (r *rabbitMQ) Consume(storage storage.TaskStorage) error {
	if r.cfg.RabbitMQDataExchange.DeclareQueue {
		_, err := r.rabbitChan.QueueDeclare(
			r.cfg.RabbitMQDataExchange.ConsumeQueueName,
			false,
			false,
			false,
			false,
			nil,
		)

		if err != nil {
			return fmt.Errorf("error in declaring queue: %w", err)
		}
	}

	msgs, err := r.rabbitChan.Consume(
		r.cfg.RabbitMQDataExchange.ConsumeQueueName,
		"",
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return fmt.Errorf("error in consuming queue: %w", err)
	}

	go r.consumeLoop(msgs, storage)

	log.Printf(" [*] Waiting for messages. To exit press CTRL+C")

	return nil
}

func (r *rabbitMQ) consumeLoop(msgs <-chan amqp.Delivery, strg storage.TaskStorage) {
	for d := range msgs {
		xFutureReceiveAtVal, ok := d.Headers[xFutureReceiveAtHeader]
		if !ok {
			r.logger.Error("Failed to get xFutureReceiveAtHeader")
			continue
		}

		var receivedAt time.Time
		if xFutureReceiveAtInt64, ok := xFutureReceiveAtVal.(int64); ok {
			receivedAt = time.UnixMilli(xFutureReceiveAtInt64)
		} else if xFutureReceiveAtUInt64, ok := xFutureReceiveAtVal.(uint64); ok {
			receivedAt = time.UnixMilli(int64(xFutureReceiveAtUInt64))
		} else if xFutureReceiveAtString, ok := xFutureReceiveAtVal.(string); ok {
			xFutureReceiveAtParsed, err := strconv.ParseInt(xFutureReceiveAtString, 10, 64)
			if err != nil {
				r.logger.Error("Failed to get xFutureReceiveAtVal")
				continue
			}

			receivedAt = time.UnixMilli(xFutureReceiveAtParsed)
		} else {
			r.logger.Error("Failed to get xFutureReceiveAtVal")
			continue
		}

		r.logger.Debug("Received a message")
		strg.Add(d.Body, receivedAt)
	}
}

func (r *rabbitMQ) Publish(payload []byte) {
	err := r.rabbitChan.PublishWithContext(context.TODO(),
		"",
		r.cfg.RabbitMQDataExchange.ProduceQueueName,
		false,
		false,
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "text/plain",
			Body:         payload,
		})
	if err != nil {
		r.logger.Error("Failed to publish a message", zap.Error(err))
	}
}

func (r *rabbitMQ) Close() {
	err := r.rabbitChan.Close()
	if err != nil {
		r.logger.Error("Failed to close rabbitMQ channel", zap.Error(err))
	}

	err = r.rabbitConn.Close()
	if err != nil {
		r.logger.Error("Failed to close rabbitMQ connection", zap.Error(err))
	}
}

const (
	xFutureReceiveAtHeader = "x-future-deliver-at"
)
