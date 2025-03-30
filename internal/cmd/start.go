/*
Copyright © 2025 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

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
	var logger *zap.Logger

	loggerConfig := zap.NewProductionConfig()
	loggerConfig.DisableCaller = true
	loggerConfig.DisableStacktrace = true
	loggerConfig.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	loggerConfig.EncoderConfig.TimeKey = "time"
	loggerConfig.EncoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder
	logger, _ = loggerConfig.Build()
	defer func() {
		_ = logger.Sync()
	}()

	cfg, err := config.PrepareConfig(configFile)
	if err != nil {
		logger.Fatal("error loading config", zap.Error(err))
	}

	// Post setup of logger after parsing the config
	lvl, err := zap.ParseAtomicLevel(cfg.Observability.Logging.Level)
	if err != nil {
		logger.With(zap.Error(err)).Error("invalid observability.logging.level, continuing with default level: info")
	} else {
		loggerConfig.Level = lvl
		logger, _ = loggerConfig.Build()
	}

	strg := storage.NewMemoryArray()

	if cfg.RabbitMQ != nil {
		rabbitmqQ := q.NewRabbitMQ(*cfg.RabbitMQ, logger.Named("rabbitmq"), strg)
		defer rabbitmqQ.Close()

		err := rabbitmqQ.Connect()
		if err != nil {
			logger.Fatal("error connecting to rabbitmq", zap.Error(err))
		}

		err = rabbitmqQ.Consume()
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
