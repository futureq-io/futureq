package dispatcher

import (
	"encoding/json"
	"fmt"
	"math"

	raft "github.com/futureq-io/futureq/internal/raft/event"
	"github.com/futureq-io/futureq/internal/storage"
)

// RestoreStandaloneEpoch runs before accepting subscriptions or starting any
// workers. Existing receipts outlive the Hub, so a new process must start above
// their epochs to retire its predecessor's ephemeral universal interests.
func (h *Hub) RestoreStandaloneEpoch(db storage.DB) (err error) {
	prefix := []byte("metadata/delivery/")
	upper := append([]byte(nil), prefix...)
	upper[len(upper)-1]++
	iter, err := db.NewIter(&storage.IterOptions{LowerBound: prefix, UpperBound: upper})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := iter.Close(); err == nil {
			err = closeErr
		}
	}()
	var epoch uint64
	for iter.First(); iter.Valid(); iter.Next() {
		var state raft.DeliveryState
		if err := json.Unmarshal(iter.Value(), &state); err != nil {
			return fmt.Errorf("read standalone delivery epoch: %w", err)
		}
		epoch = max(epoch, state.Epoch)
	}
	if err := iter.Error(); err != nil {
		return err
	}
	if epoch == math.MaxUint64 {
		return fmt.Errorf("standalone delivery epoch exhausted")
	}
	h.mu.Lock()
	h.registryEpoch = max(h.registryEpoch, epoch+1)
	h.mu.Unlock()
	return nil
}
