package leaderpersist

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

type TrackerSuite struct {
	suite.Suite
}

func TestTrackerSuite(t *testing.T) {
	suite.Run(t, new(TrackerSuite))
}

func (s *TrackerSuite) TestNotifyClosesRegisteredChannel() {
	require := s.Require()
	tr := NewTracker()

	h := tr.Hash([]byte("payload-a"))
	ch, cancel := tr.Register(h)
	defer cancel()

	tr.Notify(h)

	select {
	case <-ch:
		// closed as expected
	default:
		require.Fail("channel was not closed by Notify")
	}
}

func (s *TrackerSuite) TestNotifyDoesNotTouchOtherHashes() {
	require := s.Require()
	tr := NewTracker()

	a := tr.Hash([]byte("a"))
	b := tr.Hash([]byte("b"))

	chA, cancelA := tr.Register(a)
	defer cancelA()
	chB, cancelB := tr.Register(b)
	defer cancelB()

	tr.Notify(a)

	select {
	case <-chA:
	default:
		require.Fail("channel for hash a should have been closed")
	}
	select {
	case <-chB:
		require.Fail("channel for hash b must not be closed by Notify(a)")
	default:
	}
}

func (s *TrackerSuite) TestMultipleWaitersSameHashAllNotified() {
	require := s.Require()
	tr := NewTracker()

	h := tr.Hash([]byte("shared"))
	ch1, cancel1 := tr.Register(h)
	defer cancel1()
	ch2, cancel2 := tr.Register(h)
	defer cancel2()

	tr.Notify(h)

	for i, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case <-ch:
		default:
			require.Failf("waiter not notified", "index=%d", i)
		}
	}
}

func (s *TrackerSuite) TestCancelRemovesRegistration() {
	require := s.Require()
	tr := NewTracker()

	h := tr.Hash([]byte("x"))
	_, cancel := tr.Register(h)
	cancel()

	// After cancel, Notify should be a no-op for this hash and must not
	// panic or close anything.
	tr.Notify(h)

	require.Empty(tr.waiters)
}

func (s *TrackerSuite) TestHashIsDeterministicWithinProcess() {
	require := s.Require()
	tr := NewTracker()

	payload := []byte("deterministic")
	require.Equal(tr.Hash(payload), tr.Hash(payload))
	require.NotEqual(tr.Hash(payload), tr.Hash([]byte("other")))
}

func (s *TrackerSuite) TestNotifyWithoutWaitersIsNoop() {
	tr := NewTracker()
	require := s.Require()

	require.NotPanics(func() {
		tr.Notify(tr.Hash([]byte("nothing-registered")))
	})
}
