package raft

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/futureq-io/futureq/internal/storage"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
)

// A retry owner identifies one process on a Raft replica (node ID and UUID).
const RetryOwnerSize = 8 + AttemptTokenSize

type retryOwnerLease struct {
	owner []byte
	until int64
}

func retryOwnerKey(node uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte("metadata/retry-owner/"), node)
}

func readRetryOwner(db storage.DB, node uint64) (retryOwnerLease, error) {
	data, closer, err := db.Get(retryOwnerKey(node))
	if errors.Is(err, storage.ErrNotFound) {
		return retryOwnerLease{}, nil
	}
	if err != nil {
		return retryOwnerLease{}, err
	}
	defer closer.Close() //nolint:errcheck
	if len(data) != RetryOwnerSize+8 {
		return retryOwnerLease{}, fmt.Errorf("invalid retry owner lease")
	}
	return retryOwnerLease{owner: bytes.Clone(data[8:]), until: int64(binary.BigEndian.Uint64(data))}, nil
}

func (l retryOwnerLease) live(owner []byte, nowMs int64) bool {
	return l.until > nowMs && bytes.Equal(l.owner, owner)
}

func RetryOwnerLive(db storage.DB, owner []byte, nowMs int64) (bool, error) {
	if len(owner) == 0 {
		return false, nil
	}
	if len(owner) != RetryOwnerSize {
		return false, fmt.Errorf("invalid retry owner")
	}
	lease, err := readRetryOwner(db, binary.BigEndian.Uint64(owner))
	return lease.live(owner, nowMs), err
}

func (b *DeliveryBatch) retryOwner(node uint64) (retryOwnerLease, error) {
	if lease, ok := b.retryOwners[node]; ok {
		return lease, nil
	}
	return readRetryOwner(b.db, node)
}

func (b *DeliveryBatch) retryOwnerLive(owner []byte, nowMs int64) (bool, error) {
	if len(owner) == 0 {
		return false, nil
	}
	lease, err := b.retryOwner(binary.BigEndian.Uint64(owner))
	return lease.live(owner, nowMs), err
}

func (b *DeliveryBatch) renewRetryOwner(cmd *deliverypb.RetryCommand) (int64, error) {
	node := binary.BigEndian.Uint64(cmd.Owner)
	lease, err := b.retryOwner(node)
	if err != nil {
		return 0, err
	}
	if lease.until > cmd.NowMs && !bytes.Equal(lease.owner, cmd.Owner) {
		return 0, nil
	}
	if lease.live(cmd.Owner, cmd.NowMs) && lease.until >= cmd.LeaseUntilMs {
		return lease.until, nil
	}
	data := append(binary.BigEndian.AppendUint64(nil, uint64(cmd.LeaseUntilMs)), cmd.Owner...)
	if err := b.batch.Set(retryOwnerKey(node), data); err != nil {
		return 0, err
	}
	if b.retryOwners == nil {
		b.retryOwners = make(map[uint64]retryOwnerLease)
	}
	b.retryOwners[node] = retryOwnerLease{owner: cmd.Owner, until: cmd.LeaseUntilMs}
	return cmd.LeaseUntilMs, nil
}
