package leaderpersist

import (
	"testing"

	pb "github.com/lni/dragonboat/v4/raftpb"
	"github.com/stretchr/testify/require"
)

func TestPersistedEncodedCommandMatchesOriginalPayload(t *testing.T) {
	payload := []byte("publish-with-frozen-destinations")
	for _, test := range []struct {
		name   string
		entry  pb.Entry
		notify bool
	}{
		{"raw", pb.Entry{Type: pb.ApplicationEntry, Cmd: payload}, true},
		{"encoded", pb.Entry{Type: pb.EncodedEntry, Cmd: append([]byte{0}, payload...)}, true},
		{"unknown-version", pb.Entry{Type: pb.EncodedEntry, Cmd: append([]byte{0x10}, payload...)}, false},
		{"unsupported-compression", pb.Entry{Type: pb.EncodedEntry, Cmd: append([]byte{2}, payload...)}, false},
		{"empty-encoded", pb.Entry{Type: pb.EncodedEntry}, false},
		{"config-change", pb.Entry{Type: pb.ConfigChangeEntry, Cmd: payload}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := NewTracker()
			ch, cancel := tracker.Register(tracker.Hash(payload))
			defer cancel()
			db := &logDB{ILogDB: &stubLogDB{}, tracker: tracker}
			require.NoError(t, db.SaveRaftState([]pb.Update{{EntriesToSave: []pb.Entry{test.entry}}}, 1))
			select {
			case <-ch:
				require.True(t, test.notify)
			default:
				require.False(t, test.notify)
			}
		})
	}
}
