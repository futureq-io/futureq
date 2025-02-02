/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"log"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"

	"github.com/spf13/cobra"
)

// serverCmd represents the server command
var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the server",
	Run:   serverRun,
}

func init() {
	rootCmd.AddCommand(serverCmd)
}

func serverRun(cmd *cobra.Command, args []string) {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	logger.Info("starting server")

	conn, err := amqp.Dial("amqp://guest:guest@localhost:5672/")
	failOnError(err, "Failed to connect to RabbitMQ")
	defer conn.Close()

	ch, err := conn.Channel()
	failOnError(err, "Failed to open a channel")
	defer ch.Close()

	q, err := ch.QueueDeclare(
		"hello", // name
		false,   // durable
		false,   // delete when unused
		false,   // exclusive
		false,   // no-wait
		nil,     // arguments
	)
	failOnError(err, "Failed to declare a queue")

	msgs, err := ch.Consume(
		q.Name, // queue
		"",     // consumer
		true,   // auto-ack
		false,  // exclusive
		false,  // no-local
		false,  // no-wait
		nil,    // args
	)
	failOnError(err, "Failed to register a consumer")

	var forever chan struct{}

	go func() {
		for d := range msgs {
			xFutureReceiveAtVal, ok := d.Headers[xFutureReceiveAtHeader]
			if !ok {
				logger.Error("Failed to get xFutureReceiveAtHeader")
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
					logger.Error("Failed to get xFutureReceiveAtVal")
					continue
				}

				receivedAt = time.UnixMilli(xFutureReceiveAtParsed)
			} else {
				logger.Error("Failed to get xFutureReceiveAtVal")
				continue
			}

			logger.Info("Received a message", zap.String("receivedAt", receivedAt.String()))

		}
	}()

	log.Printf(" [*] Waiting for messages. To exit press CTRL+C")
	<-forever
}

func failOnError(err error, msg string) {
	if err != nil {
		log.Panicf("%s: %s", msg, err)
	}
}

const (
	xFutureReceiveAtHeader = "x-future-deliver-at"
)
