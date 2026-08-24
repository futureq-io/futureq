package log

import (
	"github.com/futureq-io/futureq/internal/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func InitLogger(cfg config.Logger) (*zap.Logger, error) {
	zapConfig := zap.NewProductionConfig()
	zapConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	zapConfig.EncoderConfig.TimeKey = "timestamp"

	var level zapcore.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err == nil {
		zapConfig.Level = zap.NewAtomicLevelAt(level)
	}

	logger, err := zapConfig.Build(
		zap.AddCaller(),
		zap.AddStacktrace(zapcore.ErrorLevel),
	)

	if err != nil {
		return nil, err
	}

	zap.ReplaceGlobals(logger)

	return logger, nil
}
