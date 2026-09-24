package leaderpersist

import (
	"github.com/lni/dragonboat/v4/config"
	"github.com/lni/dragonboat/v4/plugin/tan"
	"github.com/lni/dragonboat/v4/raftio"
	pb "github.com/lni/dragonboat/v4/raftpb"
)

// NewFactory returns a config.LogDBFactory compatible decorator. tracker
// must outlive the NodeHost that uses this factory.
func NewFactory(tracker *Tracker) config.LogDBFactory {
	return &factoryAdapter{tracker: tracker}
}

// factoryAdapter adapts *Factory to the config.LogDBFactory interface
// (which Dragonboat calls with Create/Name rather than exposing the struct).
type factoryAdapter struct {
	tracker *Tracker
}

func (f *factoryAdapter) Create(cfg config.NodeHostConfig,
	cb config.LogDBCallback, dirs []string, wals []string) (raftio.ILogDB, error) {
	inner, err := tan.Factory.Create(cfg, cb, dirs, wals)
	if err != nil {
		return nil, err
	}
	return &logDB{ILogDB: inner, tracker: f.tracker}, nil
}

func (f *factoryAdapter) Name() string {
	return tan.Factory.Name() + "+leaderpersist"
}

// logDB decorates the default ILogDB, hooking SaveRaftState to notify the
// tracker. All other methods delegate verbatim.
type logDB struct {
	raftio.ILogDB
	tracker *Tracker
}

// SaveRaftState delegates the write to the wrapped ILogDB. On success it
// hashes every persisted entry's Cmd payload and notifies the tracker —
// this is the moment the leader has durably written the entry to its own
// Raft log, before replication to a quorum completes.
//
// The method is called by a single Dragonboat worker per shard, so the
// per-update loop below never races another SaveRaftState for the same
// shard. Different shards may run on different workers; the Tracker is
// safe for that.
func (l *logDB) SaveRaftState(updates []pb.Update, shardID uint64) error {
	if err := l.ILogDB.SaveRaftState(updates, shardID); err != nil {
		return err
	}

	for i := range updates {
		ud := &updates[i]
		for j := range ud.EntriesToSave {
			cmd := ud.EntriesToSave[j].Cmd
			if len(cmd) == 0 {
				continue
			}
			l.tracker.Notify(l.tracker.Hash(cmd))
		}
	}

	return nil
}
