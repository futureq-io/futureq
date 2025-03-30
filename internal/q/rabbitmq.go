package q

import (
	"context"
	"errors"
	"fmt"
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
	storage    storage.TaskStorage
}

func NewRabbitMQ(cfg config.RabbitMQ, logger *zap.Logger, taskStorage storage.TaskStorage) Q {
	return &rabbitMQ{
		cfg:     cfg,
		logger:  logger,
		storage: taskStorage,
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

func (r *rabbitMQ) Consume() error {
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

	deliveryChan, err := r.rabbitChan.Consume(
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

	go r.consumeLoop(deliveryChan, r.cfg.RabbitMQDataExchange.ConsumeQueueName)

	return nil
}

func (r *rabbitMQ) consumeLoop(deliveryChan <-chan amqp.Delivery, queue string) {
	for delivery := range deliveryChan {

		startedAt := time.Now()
		err := r.handleDelivery(delivery)
		duration := time.Since(startedAt)

		log := r.logger.With(
			zap.String("exchange", delivery.Exchange),
			zap.String("queue", queue),
			zap.String("routing_key", delivery.RoutingKey),
			zap.String("consumer_tag", delivery.ConsumerTag),
			zap.Uint64("delivery_tag", delivery.DeliveryTag),
			zap.String("message_id", delivery.MessageId),
			zap.String("user_id", delivery.UserId),
			zap.String("app_id", delivery.AppId),
			zap.Error(err),
			zap.String("duration", duration.String()),
		)
		if err != nil {
			log.Error("error in processing message")
		} else {
			log.Debug("message processed successfully")
		}
	}
}

func (r *rabbitMQ) handleDelivery(delivery amqp.Delivery) error {
	xFutureReceiveAtVal, ok := delivery.Headers[xFutureReceiveAtHeader]
	if !ok {
		return ErrReceivedAtHeaderNotExists
	}

	var receivedAt time.Time
	if xFutureReceiveAtInt64, ok := xFutureReceiveAtVal.(int64); ok {
		receivedAt = time.UnixMilli(xFutureReceiveAtInt64)
	} else if xFutureReceiveAtUInt64, ok := xFutureReceiveAtVal.(uint64); ok {
		receivedAt = time.UnixMilli(int64(xFutureReceiveAtUInt64))
	} else if xFutureReceiveAtString, ok := xFutureReceiveAtVal.(string); ok {
		xFutureReceiveAtParsed, err := strconv.ParseInt(xFutureReceiveAtString, 10, 64)
		if err != nil {
			return ErrInvalidReceivedAtHeaderFormat
		}

		receivedAt = time.UnixMilli(xFutureReceiveAtParsed)
	} else {
		return ErrInvalidReceivedAtHeaderFormat
	}

	r.storage.Add(delivery.Body, receivedAt)

	return nil
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

var (
	ErrReceivedAtHeaderNotExists     = errors.New("received at header does not exist")
	ErrInvalidReceivedAtHeaderFormat = errors.New("invalid received at header format")
)
