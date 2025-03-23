/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"go.uber.org/zap"

	"github.com/spf13/cobra"

	"github.com/futureq-io/futureq/internal/config"
	"github.com/futureq-io/futureq/internal/q"
	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/internal/ticker"
)

// startCmd represents the server command
var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the server",
	Run:   startRun,
}

var (
	configFile *string
)

func init() {
	configFile = startCmd.Flags().StringP("config", "c", "", "Path to config file")

	rootCmd.AddCommand(startCmd)
}

func startRun(_ *cobra.Command, _ []string) {
	logger, _ := zap.NewDevelopment()
	defer func() {
		_ = logger.Sync()
	}()

	cfg, err := config.PrepareConfig(configFile)
	if err != nil {
		logger.Fatal("error loading config", zap.Error(err))
	}

	strg := storage.NewMemoryArray()

	if cfg.RabbitMQ != nil {
		rabbitmqQ := q.NewRabbitMQ(*cfg.RabbitMQ, logger.Named("rabbitmq"))
		defer rabbitmqQ.Close()

		err := rabbitmqQ.Connect()
		if err != nil {
			logger.Fatal("error connecting to rabbitmq", zap.Error(err))
		}

		err = rabbitmqQ.Consume(strg)
		if err != nil {
			logger.Fatal("error consuming rabbitmq", zap.Error(err))
		}

		t := ticker.NewTicker(strg, rabbitmqQ)

		go t.Tick()
	}

	logger.Info("starting server")
	var forever chan struct{}

	<-forever
}
