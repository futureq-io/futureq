package metadata

import (
	"bytes"
	"context"
	"testing"

	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestConsumerLivenessPreservesPermitsAndUsesExistingDrain(t *testing.T) {
	sm := newSM()
	applyConsumerChange(t, sm, "add", "a", 1)
	applyConsumerChange(t, sm, "add", "b", 2)
	finishConsumerRebalance(t, sm)
	epoch, _ := sm.ConsumerVersion()
	barriers := 0
	sm.SetConsumerBarrier(func(apply func()) { barriers++; apply() })
	svc := NewService(nil, func(_ context.Context, cmd []byte) error {
		_, err := sm.Update(statemachine.Entry{Cmd: cmd})
		return err
	}, zap.NewNop())

	// A heartbeat must neither invalidate an active permit nor change its epoch.
	cmd, err := marshalConsumerAck(0, 1)
	require.NoError(t, err)
	cmd[0] = byte(ConsumerHeartbeatCmd)
	_, err = sm.Update(statemachine.Entry{Cmd: cmd})
	require.NoError(t, err)
	current, pending, beats := sm.ConsumerHeartbeats()
	require.Equal(t, epoch, current)
	require.False(t, pending)
	require.Equal(t, map[uint64]uint64{1: 1, 2: 0}, beats)
	require.Zero(t, barriers)
	beats[1] = 99
	_, _, beats = sm.ConsumerHeartbeats()
	require.Equal(t, uint64(1), beats[1], "callers cannot mutate the replicated counters")

	var snapshot bytes.Buffer
	require.NoError(t, sm.SaveSnapshot(&snapshot, nil, nil))
	restored := newSM()
	require.NoError(t, restored.RecoverFromSnapshot(&snapshot, nil, nil))
	_, _, restoredBeats := restored.ConsumerHeartbeats()
	require.Equal(t, beats, restoredBeats)

	require.NoError(t, svc.ProbeConsumers(context.Background(), epoch))
	probeEpoch, active := sm.ConsumerVersion()
	require.Equal(t, epoch+1, probeEpoch)
	require.False(t, active)
	require.Empty(t, sm.ConsumerGroup("orders", "workers"))
	// A duplicate or delayed probe must not restart the fence clock.
	require.NoError(t, svc.ProbeConsumers(context.Background(), epoch))
	require.NoError(t, svc.ProbeConsumers(context.Background(), probeEpoch))
	current, _ = sm.ConsumerVersion()
	require.Equal(t, probeEpoch, current)
	ack, err := marshalConsumerAck(probeEpoch, 1)
	require.NoError(t, err)
	_, err = sm.Update(statemachine.Entry{Cmd: ack})
	require.NoError(t, err)
	require.NoError(t, svc.FenceConsumers(context.Background(), probeEpoch))
	require.Equal(t, []ConsumerMember{{ID: "a", NodeID: 1}}, sm.ConsumerGroup("orders", "workers"))
	require.NoError(t, svc.ProbeConsumers(context.Background(), epoch))
	_, active = sm.ConsumerVersion()
	require.True(t, active, "a delayed probe cannot freeze the new assignment")
}
