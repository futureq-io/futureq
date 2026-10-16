package config

import (
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestFullMessageCacheSettings(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	require.Zero(t, cfg.Delivery.PreloadLookahead, "future speculation remains opt-in after the performance evaluation")
	require.Equal(t, 512, cfg.Delivery.PreloadMaxMessages, "full payloads need a smaller count than the former metadata cache")
	t.Setenv("FUTUREQ_DELIVERY_PRELOADLOOKAHEAD", "100ms")
	t.Setenv("FUTUREQ_DELIVERY_PRELOADMAXMESSAGES", "17")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, 100*time.Millisecond, cfg.Delivery.PreloadLookahead)
	require.Equal(t, 17, cfg.Delivery.PreloadMaxMessages)
	t.Setenv("FUTUREQ_DELIVERY_PRELOADMAXMESSAGES", "0")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Zero(t, cfg.Delivery.PreloadMaxMessages)
	require.Positive(t, cfg.Delivery.PrepareWorkers)
	require.Positive(t, cfg.Delivery.PrepareBatchSize)
}
