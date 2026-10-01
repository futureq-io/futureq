package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConsumerQueueConfiguration(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	require.Equal(t, 1024, cfg.Delivery.ConsumerQueueSize)
	t.Setenv("FUTUREQ_DELIVERY_CONSUMERQUEUESIZE", "2048")
	cfg, err = Load("")
	require.NoError(t, err)
	require.Equal(t, 2048, cfg.Delivery.ConsumerQueueSize)
	for _, value := range []string{"0", "-1", "65537"} {
		t.Setenv("FUTUREQ_DELIVERY_CONSUMERQUEUESIZE", value)
		_, err = Load("")
		require.ErrorContains(t, err, "consumerQueueSize")
	}
}
