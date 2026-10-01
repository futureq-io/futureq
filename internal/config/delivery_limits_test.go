package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBoundedDeliveryLimitsRejectUnboundedValues(t *testing.T) {
	for _, change := range []func(*Delivery){
		func(d *Delivery) { d.PrepareBatchSize = 65 },
		func(d *Delivery) { d.ScanMaxKeys = 1 },
		func(d *Delivery) { d.PrepareBatchBytes = "1MiB" },
		func(d *Delivery) { d.ScanWorkBudget = time.Second },
		func(d *Delivery) { d.PrepareTimeout = 2 * time.Second },
		func(d *Delivery) { d.PrepareWorkers = 0 },
	} {
		cfg := defaultConfig
		change(&cfg.Delivery)
		require.Error(t, cfg.validatePublishAndDelivery())
	}
}
