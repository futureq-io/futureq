package metadata

import (
	"bytes"
	"testing"

	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
)

func applyConsumerChange(t *testing.T, sm *MetadataStateMachine, op, id string, node uint64) {
	t.Helper()
	cmd, err := marshalConsumerChange(consumerChange{
		Operation: op, Topic: "orders", Group: "workers",
		Member: ConsumerMember{ID: id, NodeID: node}, Required: []uint64{1, 2},
	})
	require.NoError(t, err)
	_, err = sm.Update(statemachine.Entry{Cmd: cmd})
	require.NoError(t, err)
}

func finishConsumerRebalance(t *testing.T, sm *MetadataStateMachine) {
	t.Helper()
	epoch, pending, _ := sm.ConsumerStatus(1)
	require.True(t, pending)
	for _, node := range []uint64{1, 2} {
		cmd, err := marshalConsumerAck(epoch, node)
		require.NoError(t, err)
		_, err = sm.Update(statemachine.Entry{Cmd: cmd})
		require.NoError(t, err)
		if node == 1 {
			require.Empty(t, sm.ConsumerGroup("orders", "workers"))
		}
	}
}

func TestConsumerMembershipBarrierAndOrdering(t *testing.T) {
	sm := newSM()
	for _, member := range []ConsumerMember{{"a", 1}, {"b", 1}, {"c", 2}} {
		applyConsumerChange(t, sm, "add", member.ID, member.NodeID)
	}
	require.Empty(t, sm.ConsumerGroup("orders", "workers"))
	finishConsumerRebalance(t, sm)
	require.Equal(t, []ConsumerMember{{"a", 1}, {"b", 1}, {"c", 2}}, sm.ConsumerGroup("orders", "workers"))

	applyConsumerChange(t, sm, "add", "d", 2)
	require.False(t, sm.ConsumerActive("orders", "workers", "d"))
	finishConsumerRebalance(t, sm)
	require.Equal(t, []ConsumerMember{{"a", 1}, {"b", 1}, {"c", 2}, {"d", 2}}, sm.ConsumerGroup("orders", "workers"))

	applyConsumerChange(t, sm, "remove", "d", 2)
	applyConsumerChange(t, sm, "add", "e", 1)
	finishConsumerRebalance(t, sm)
	require.Equal(t, []ConsumerMember{{"a", 1}, {"b", 1}, {"e", 1}, {"c", 2}}, sm.ConsumerGroup("orders", "workers"))
}

func TestConsumerMembershipSnapshotAndReconcile(t *testing.T) {
	sm := newSM()
	applyConsumerChange(t, sm, "add", "orphan", 1)
	finishConsumerRebalance(t, sm)
	var buf bytes.Buffer
	require.NoError(t, sm.SaveSnapshot(&buf, nil, nil))

	recovered := newSM()
	require.NoError(t, recovered.RecoverFromSnapshot(&buf, nil, nil))
	require.True(t, recovered.ConsumerActive("orders", "workers", "orphan"))
	cmd, err := marshalConsumerChange(consumerChange{Operation: "reconcile", NodeID: 1, Required: []uint64{1, 2}})
	require.NoError(t, err)
	_, err = recovered.Update(statemachine.Entry{Cmd: cmd})
	require.NoError(t, err)
	require.Empty(t, recovered.ConsumerGroup("orders", "workers"))
	finishConsumerRebalance(t, recovered)
	require.False(t, recovered.ConsumerActive("orders", "workers", "orphan"))
}

func TestConsumerMembershipPrunesDepartedNode(t *testing.T) {
	sm := newSM()
	applyConsumerChange(t, sm, "add", "a", 1)
	applyConsumerChange(t, sm, "add", "b", 2)
	finishConsumerRebalance(t, sm)
	topo, err := MarshalUpdateTopologyCmd(&ShardTopology{ShardID: 42, Nodes: map[uint64]string{1: "raft-1"}})
	require.NoError(t, err)
	_, err = sm.Update(statemachine.Entry{Cmd: topo})
	require.NoError(t, err)
	require.Empty(t, sm.ConsumerGroup("orders", "workers"))
	epoch, pending, _ := sm.ConsumerStatus(1)
	require.True(t, pending)
	ack, err := marshalConsumerAck(epoch, 1)
	require.NoError(t, err)
	_, err = sm.Update(statemachine.Entry{Cmd: ack})
	require.NoError(t, err)
	require.Equal(t, []ConsumerMember{{"a", 1}}, sm.ConsumerGroup("orders", "workers"))
}
