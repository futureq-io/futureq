package dispatcher

import (
	"context"
	"errors"
	"testing"
	"time"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/lni/dragonboat/v4/statemachine"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Exercise the production ledger/backend path: a blocked Raft proposal must
// neither serialize independent batches nor release their tracking prematurely.
func TestConcurrentDeletionFlushes(t *testing.T) {
	for _, kind := range []string{"ack", "delete"} {
		t.Run(kind, func(t *testing.T) {
			type proposal struct {
				cmd  []byte
				done chan error
			}
			started := make(chan proposal, 8)
			release := make(chan struct{})
			propose := func(cmd []byte) (statemachine.Result, error) {
				p := proposal{cmd: cmd, done: make(chan error, 1)}
				started <- p
				select {
				case err := <-p.done:
					return statemachine.Result{}, err
				case <-release:
					return statemachine.Result{}, errors.New("test stopping")
				}
			}
			backend := NewRaftDeleteBackend(func(cmd []byte) error {
				_, err := propose(cmd)
				return err
			}, zap.NewNop())
			ledger := NewDeliveryLedger(nil, nil, backend, propose)
			d := NewDeleter(ledger, time.Millisecond, zap.NewNop())
			d.MaxConcurrentFlushes = 2
			d.AcknowledgeBatch = ledger.Acknowledge
			completed := make(chan byte, 8)
			d.OnAcknowledged = func(acks []raft.DeliveryAck) {
				for _, ack := range acks {
					completed <- ack.Key[23]
				}
			}
			d.OnDelete = func(key []byte) { completed <- key[23] }
			enqueue := func(id byte) {
				key := make([]byte, 24)
				key[23] = id
				if kind == "ack" {
					require.True(t, d.TryMarkAcknowledged(key, "g:a"))
				} else {
					require.True(t, d.TryMarkDeleted(key))
				}
			}
			next := func() proposal {
				select {
				case p := <-started:
					return p
				case <-time.After(time.Second):
					t.Fatal("independent batch did not reach Raft")
					return proposal{}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { d.Run(ctx); close(done) }()
			t.Cleanup(func() {
				cancel()
				close(release)
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("deleter did not stop")
				}
			})

			enqueue(1)
			first := next()
			enqueue(2)
			second := next() // First proposal is still blocked.
			enqueue(3)
			select {
			case <-started:
				t.Fatal("exceeded configured concurrency")
			case <-time.After(20 * time.Millisecond):
			}
			require.Empty(t, completed, "submission must not release tracking")
			d.mu.Lock()
			tracked := len(d.ackKeys) + len(d.deleteKeys)
			d.mu.Unlock()
			require.Equal(t, 3, tracked, "queued and in-progress work remain tracked")

			first.done <- nil
			select {
			case id := <-completed:
				require.Equal(t, byte(1), id)
			case <-time.After(time.Second):
				t.Fatal("successful batch did not release tracking")
			}
			third := next()
			second.done <- errors.New("no quorum")
			retry := next()
			require.Equal(t, second.cmd, retry.cmd, "failed batch must be retried")
			require.Empty(t, completed, "failed batch must retain tracking")

			cancel()
			select {
			case <-done:
				t.Fatal("shutdown abandoned outstanding proposals")
			case <-time.After(20 * time.Millisecond):
			}
			third.done <- nil
			retry.done <- nil
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("shutdown did not drain proposals")
			}
			require.Len(t, completed, 2)
			require.Empty(t, started, "completed batches must not be resubmitted")
			require.Empty(t, d.ackKeys)
			require.Empty(t, d.deleteKeys)
			require.Empty(t, d.acks)
			require.Empty(t, d.pending)
		})
	}
}
