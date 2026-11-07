package dispatcher

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/futureq-io/futureq/pkg/raft/metadata"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRepairRevisitsOlderInsertWhileTailKeepsGrowing(t *testing.T) {
	d, _ := newPipelineTest(t)
	d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 1))
	d.Limits.Candidates, d.Limits.Examined, d.Limits.WorkTime = 1, 1, time.Hour
	due := time.Now().Add(-time.Second).UnixMilli()
	for id := uint64(1); id <= 4; id++ {
		putPipelineEvent(t, d, "orders", id, due)
	}
	require.Len(t, d.collectTopic("orders", time.Now().UnixMilli()).items, 1)
	older := putPipelineEvent(t, d, "orders", 0, due-1000)
	foundOlder, foundTail := false, false
	tail := putPipelineEvent(t, d, "orders", 5, due)
	for step := 0; step < 30; step++ {
		// The same due bucket is deliberate: freezing just the clock cutoff is insufficient.
		putPipelineEvent(t, d, "orders", uint64(6+step), due)
		for _, item := range d.collectTopic("orders", time.Now().UnixMilli()).items {
			foundOlder = foundOlder || bytes.Equal(older, item.Key)
			foundTail = foundTail || bytes.Equal(tail, item.Key)
		}
		if step == 4 {
			require.True(t, foundOlder, "a growing tail must not postpone the next repair pass")
		}
	}
	require.True(t, foundTail, "newer keys must also make progress across repair passes")
}

func TestDiscoveryUsesLivePreloadKeysAndBoundedAssignedBatch(t *testing.T) {
	d, _ := newPipelineTest(t)
	d.hub.Register("local", "orders", "workers", make(chan *pb.QueueMessage, 8))
	d.Limits.WorkTime = time.Hour
	due := time.Now().Add(-time.Second).UnixMilli()
	keys := make([][]byte, 3)
	p := &topicPreload{epoch: d.hub.InterestView("orders").Epoch}
	for i := range keys {
		keys[i] = putPipelineEvent(t, d, "orders", uint64(i+1), due)
		p.addKey([24]byte(keys[i]))
	}
	d.db = scanStartupDB{DB: d.db, beforeFirst: func() { p.removeKey([24]byte(keys[0])); p.removeKey([24]byte(keys[2])) }}
	// Exercise concurrent index updates as discovery reads it, without sharing heaps.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				p.addKey([24]byte{99})
				p.removeKey([24]byte{99})
			}
		}
	})
	result := d.discover(discoveryJob{topic: "orders", owner: p, dueSlots: 3, assigned: map[[24]byte]bool{[24]byte(keys[2]): true}})
	close(stop)
	wg.Wait()
	require.Len(t, result.chunk.items, 1)
	require.Equal(t, keys[0], result.chunk.items[0].Key, "removed preload entries must become discoverable while cached and assigned keys stay excluded")
}

func TestMetadataNoOpsPreservePermitAndChangesStillFence(t *testing.T) {
	sm := metadata.NewMetadataStateMachineFactory(zap.NewNop())(metadata.MetadataShardID, 1).(*metadata.MetadataStateMachine)
	h := NewHub(NewRoundRobinStrategy(), zap.NewNop(), make(chan struct{}, 1))
	h.SetDeliveryFence(sm.ConsumerVersion, time.Minute)
	sm.SetConsumerBarrier(h.WithConsumerBarrier)
	apply := func(kind metadata.CommandType, value any) statemachine.Result {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		result, err := sm.Update(statemachine.Entry{Cmd: append([]byte{byte(kind)}, data...)})
		require.NoError(t, err)
		return result
	}
	add := map[string]any{"operation": "add", "topic": "orders", "group": "workers", "member": map[string]any{"id": "a", "node_id": 1}, "required": []uint64{1}}
	apply(metadata.ConsumerChangeCmd, add)
	epoch, active := sm.ConsumerVersion()
	require.False(t, active)
	apply(metadata.ConsumerAckCmd, map[string]any{"epoch": epoch, "node_id": 1})
	h.GrantDeliveryPermit(epoch, time.Now())
	require.True(t, h.DeliveryPermitted())
	for _, c := range []struct {
		kind  metadata.CommandType
		value any
	}{
		{metadata.ConsumerChangeCmd, add},
		{metadata.ConsumerChangeCmd, map[string]any{"operation": "remove", "topic": "orders", "group": "workers", "member": map[string]any{"id": "missing"}, "required": []uint64{1}}},
		{metadata.ConsumerChangeCmd, map[string]any{"operation": "reconcile", "node_id": 1, "local_ids": []string{"a"}, "required": []uint64{1}}},
		{metadata.ConsumerProbeCmd, map[string]any{"epoch": epoch - 1, "node_id": 1}},
		{metadata.ConsumerFenceCmd, map[string]any{"epoch": epoch, "node_id": 1}},
	} {
		apply(c.kind, c.value)
		require.True(t, h.DeliveryPermitted(), "no-op %v must preserve the permit", c.kind)
	}
	// Address/leader-only topology updates leave consumer ownership unchanged.
	topo, err := metadata.MarshalUpdateTopologyCmd(&metadata.ShardTopology{ShardID: 42, LeaderID: 1, Nodes: map[uint64]string{1: "raft-1"}})
	require.NoError(t, err)
	_, err = sm.Update(statemachine.Entry{Cmd: topo})
	require.NoError(t, err)
	require.True(t, h.DeliveryPermitted(), "unchanged consumer topology must preserve the permit")
	// Snapshot replacement always fences even if the restored epoch is identical.
	var snapshot bytes.Buffer
	require.NoError(t, sm.SaveSnapshot(&snapshot, nil, nil))
	require.NoError(t, sm.RecoverFromSnapshot(&snapshot, nil, nil))
	require.False(t, h.DeliveryPermitted())
	h.GrantDeliveryPermit(epoch, time.Now())
	require.True(t, h.DeliveryPermitted())
	apply(metadata.ConsumerProbeCmd, map[string]any{"epoch": epoch, "node_id": 1})
	require.False(t, h.DeliveryPermitted(), "a real ownership transition still fences immediately")
	nextEpoch, active := sm.ConsumerVersion()
	require.Equal(t, epoch+1, nextEpoch)
	require.False(t, active)
	// Activation cannot resurrect the old permit.
	apply(metadata.ConsumerAckCmd, map[string]any{"epoch": nextEpoch, "node_id": 1})
	require.False(t, h.DeliveryPermitted())
	h.GrantDeliveryPermit(nextEpoch, time.Now())
	require.True(t, h.DeliveryPermitted())
	apply(metadata.ConsumerChangeCmd, map[string]any{"operation": "remove", "topic": "orders", "group": "workers", "member": map[string]any{"id": "a"}, "required": []uint64{1}})
	require.False(t, h.DeliveryPermitted(), "an actual membership removal still revokes permission")
}
