package raft

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"github.com/stretchr/testify/require"
)

type pausedCatalogSnapshot struct {
	io.Reader
	once    sync.Once
	started chan struct{}
	resume  chan struct{}
}

func (r *pausedCatalogSnapshot) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.started)
		<-r.resume
	})
	return r.Reader.Read(p)
}

func TestTopicCatalogRejectsReadsUntilSnapshotRecoveryCompletes(t *testing.T) {
	sm, _ := newDeliveryTestSM(t)
	cmd, err := MarshalSubscriptionCommand(&deliverypb.SubscriptionCommand{
		Operation: TopicCreate, Topic: "orders", NowMs: 1000,
		Mutations: []*deliverypb.SubscriptionMutation{{Topic: "orders", GroupId: "workers", Recipient: "g:s:workers"}},
	})
	require.NoError(t, err)
	applyAdmissionSequence(t, sm, cmd)

	view, err := sm.PrepareSnapshot()
	require.NoError(t, err)
	var snapshot bytes.Buffer
	require.NoError(t, sm.SaveSnapshot(view, &snapshot, nil))

	get := TopicCatalogQuery{Name: "orders"}
	list := TopicCatalogQuery{Limit: 10}
	publish := PublishRecipientsQuery{"orders"}
	checkCatalog := func() {
		t.Helper()
		result, err := sm.Lookup(get)
		require.NoError(t, err)
		require.Equal(t, TopicCatalogResult{Exists: true}, result)
		result, err = sm.Lookup(list)
		require.NoError(t, err)
		require.Equal(t, TopicCatalogResult{Names: []string{"orders"}}, result)
		result, err = sm.Lookup(publish)
		require.NoError(t, err)
		require.Equal(t, PublishRecipientsResult{Recipients: map[string][]string{"orders": {"g:s:workers"}}}, result)
	}
	checkUnavailable := func() {
		t.Helper()
		for _, query := range []any{get, list, publish} {
			_, err := sm.Lookup(query)
			require.ErrorIs(t, err, errTopicCatalogUnavailable)
		}
	}
	checkCatalog()

	reader := &pausedCatalogSnapshot{
		Reader:  bytes.NewReader(snapshot.Bytes()),
		started: make(chan struct{}),
		resume:  make(chan struct{}),
	}
	resume := sync.OnceFunc(func() { close(reader.resume) })
	done := make(chan struct{})
	recovered := make(chan error, 1)
	go func() {
		defer close(done)
		recovered <- sm.RecoverFromSnapshot(reader, nil)
	}()
	t.Cleanup(func() {
		resume()
		<-done
	})
	select {
	case <-reader.started:
	case err := <-recovered:
		t.Fatalf("recovery finished before reading the snapshot: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not start reading the snapshot")
	}

	// Recovery has cleared storage and has yet to read the first snapshot key.
	checkUnavailable()
	resume()
	require.NoError(t, <-recovered)
	checkCatalog()

	require.ErrorIs(t, sm.RecoverFromSnapshot(bytes.NewReader([]byte{1}), nil), io.ErrUnexpectedEOF)
	checkUnavailable()
	require.NoError(t, sm.RecoverFromSnapshot(bytes.NewReader(snapshot.Bytes()), nil))
	checkCatalog()
}
