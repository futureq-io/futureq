package dispatcher

import (
	"testing"
	"time"

	"github.com/futureq-io/futureq/internal/storage"
	pb "github.com/futureq-io/protocol/proto/go"
	"github.com/stretchr/testify/require"
)

type scanStartupDB struct {
	storage.DB
	beforeNewIter func()
	beforeFirst   func()
}

func (db scanStartupDB) NewIter(opts *storage.IterOptions) (storage.Iterator, error) {
	if db.beforeNewIter != nil {
		db.beforeNewIter()
	}
	iter, err := db.DB.NewIter(opts)
	if err != nil {
		return nil, err
	}
	return scanStartupIterator{Iterator: iter, beforeFirst: db.beforeFirst}, nil
}

type scanStartupIterator struct {
	storage.Iterator
	beforeFirst func()
}

func (iter scanStartupIterator) First() bool {
	if iter.beforeFirst != nil {
		iter.beforeFirst()
	}
	return iter.Iterator.First()
}

func newScanProgressTest(t *testing.T, maintenance bool) (*Dispatcher, [][]byte, func() deliveryChunk, string) {
	t.Helper()
	d, ledger := newPipelineTest(t)
	d.hub.Register("local", "orders", "", make(chan *pb.QueueMessage, 3))
	var keys [][]byte
	for id := uint64(1); id <= 3; id++ {
		key := putPipelineEvent(t, d, "orders", id, time.Now().Add(-time.Second).UnixMilli())
		keys = append(keys, key)
		if maintenance {
			_, err := ledger.Prepare("orders", key)
			require.NoError(t, err)
		}
	}
	if maintenance {
		d.hub.Unregister("local") // Make the prepared receipts stale.
		return d, keys, d.collectMaintenance, maintenanceTopic
	}
	return d, keys, func() deliveryChunk { return d.collectTopic("orders", time.Now().UnixMilli()) }, "orders"
}

func TestScanProgressWhenStartupExceedsBudget(t *testing.T) {
	for _, maintenance := range []bool{false, true} {
		name := "delivery"
		if maintenance {
			name = "maintenance"
		}
		t.Run(name, func(t *testing.T) {
			for _, stage := range []string{"NewIter", "First"} {
				t.Run(stage, func(t *testing.T) {
					d, keys, collect, cursor := newScanProgressTest(t, maintenance)
					d.Limits.WorkTime = time.Millisecond
					db := scanStartupDB{DB: d.db}
					delay := func() { time.Sleep(2 * d.Limits.WorkTime) }
					if stage == "NewIter" {
						db.beforeNewIter = delay
					} else {
						db.beforeFirst = delay
					}
					d.db = db
					for i, key := range keys {
						chunk := collect()
						require.Len(t, chunk.items, 1, "make progress, then yield on the exhausted time budget")
						require.Equal(t, key, chunk.items[0].Key, "successive scans must visit successive keys")
						if i < len(keys)-1 {
							require.NotEmpty(t, d.cursors[cursor].after)
						}
					}
					require.Empty(t, d.cursors[cursor].after, "reaching the end resets the cyclic cursor")
					require.Equal(t, keys[0], collect().items[0].Key, "a completed pass must revisit earlier keys")
				})
			}
		})
	}
}

func TestScanProgressDoesNotBypassRevokedPermit(t *testing.T) {
	for _, maintenance := range []bool{false, true} {
		name := "delivery"
		if maintenance {
			name = "maintenance"
		}
		t.Run(name, func(t *testing.T) {
			d, _, collect, cursor := newScanProgressTest(t, maintenance)
			epoch, _ := d.hub.DeliveryEpoch()
			d.hub.SetDeliveryFence(func() (uint64, bool) { return epoch, true }, time.Second)
			d.hub.GrantDeliveryPermit(epoch, time.Now())
			d.Limits.WorkTime = time.Nanosecond
			d.db = scanStartupDB{DB: d.db, beforeFirst: func() {
				d.hub.WithRebalanceBarrier(func() {})
			}}
			require.Empty(t, collect().items)
			require.Empty(t, d.cursors[cursor].after, "revocation during startup must stop even the first key")
		})
	}
}
