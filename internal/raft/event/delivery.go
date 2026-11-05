package raft

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/lni/dragonboat/v4/statemachine"
)

// DeliveryState shares one payload across independent logical deliveries.
// Named group receipts survive disconnects and reassignment. Universal
// subscriptions are ephemeral and lose their interest when unregistered.
type DeliveryState struct {
	Epoch        uint64
	Recipients   []string // sorted, unique frozen roster
	Acknowledged []byte   // bit i corresponds to Recipients[i]
	remaining    int      // derived on read; updated only by the first ACK for a recipient
}

func (s *DeliveryState) Needs(recipient string) bool {
	if s == nil {
		return false
	}
	i := sort.SearchStrings(s.Recipients, recipient)
	return i < len(s.Recipients) && s.Recipients[i] == recipient && (i/8 >= len(s.Acknowledged) || s.Acknowledged[i/8]&(1<<uint(i%8)) == 0)
}

func (s *DeliveryState) complete() bool { return s.remaining == 0 }

func (s *DeliveryState) acknowledge(recipient string) bool {
	if !s.Needs(recipient) {
		return false
	}
	i := sort.SearchStrings(s.Recipients, recipient)
	s.Acknowledged[i/8] |= 1 << uint(i%8)
	s.remaining--
	return true
}

type DeliveryAck struct {
	Key       []byte `json:"key"`
	Recipient string `json:"recipient"`
}

type DeliveryPrepare struct {
	Key        []byte   `json:"key"`
	Epoch      uint64   `json:"epoch"`
	Recipients []string `json:"recipients"`
}

type prepareDelivery = DeliveryPrepare // old single-key command wire shape

const (
	MaxDeliveryPrepareItems = 256
	MaxDeliveryPrepareBytes = 256 << 10
	MaxDeliveryRecipients   = 1024
)

// DeliveryPrepared identifies every input, including payloads skipped because
// they were deleted or had no recipients. Results come from the committed
// state machine; callers need not assume an immediate follower Get is current.
type DeliveryPrepared struct {
	Key   []byte         `json:"key"`
	State *DeliveryState `json:"state"`
}

func validatePrepares(items []DeliveryPrepare) error {
	if len(items) == 0 || len(items) > MaxDeliveryPrepareItems {
		return fmt.Errorf("delivery prepare batch must contain 1..%d items", MaxDeliveryPrepareItems)
	}
	seen := make(map[[24]byte]bool, len(items))
	views := make(map[prepareViewKey]bool)
	for _, item := range items {
		if len(item.Key) != 24 || seen[[24]byte(item.Key)] {
			return fmt.Errorf("invalid or duplicate delivery key")
		}
		seen[[24]byte(item.Key)] = true
		if len(item.Recipients) > MaxDeliveryRecipients {
			return fmt.Errorf("too many delivery recipients")
		}
		view := prepareView(item)
		if views[view] {
			continue
		}
		views[view] = true
		recipients := make(map[string]bool, len(item.Recipients))
		for _, recipient := range item.Recipients {
			recipient = deliveryRecipient(recipient)
			if len(recipient) <= 2 || (!strings.HasPrefix(recipient, "g:") && !strings.HasPrefix(recipient, "u:")) || recipients[recipient] {
				return fmt.Errorf("invalid or duplicate delivery recipient")
			}
			recipients[recipient] = true
		}
	}
	return nil
}

func MarshalPrepareDeliveryBatchCmd(items []DeliveryPrepare) ([]byte, error) {
	return marshalProtoPrepares(items, ProtoPrepareDeliveryBatchCmd)
}

// Decode incrementally and check the count before allocating another item.
// Validation completes before any mutations are added to the overlay.
func UnmarshalPrepareDeliveryBatchCmd(cmd []byte) ([]DeliveryPrepare, error) {
	if len(cmd) > 0 && (CommandType(cmd[0]) == ProtoPrepareDeliveryBatchCmd || CommandType(cmd[0]) == ProtoPrepareDeliveryCmd) {
		return unmarshalProtoPrepares(cmd)
	}
	if len(cmd) < 3 || len(cmd) > MaxDeliveryPrepareBytes || CommandType(cmd[0]) != PrepareDeliveryBatchCmd {
		return nil, fmt.Errorf("invalid delivery prepare batch size or tag")
	}
	dec := json.NewDecoder(bytes.NewReader(cmd[1:]))
	dec.DisallowUnknownFields()
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		return nil, fmt.Errorf("delivery prepare batch must be an array")
	}
	items := make([]DeliveryPrepare, 0, MaxDeliveryPrepareItems)
	for dec.More() {
		if len(items) == MaxDeliveryPrepareItems {
			return nil, fmt.Errorf("too many delivery prepare items")
		}
		var item DeliveryPrepare
		if err := dec.Decode(&item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("trailing delivery prepare data")
	}
	return items, validatePrepares(items)
}

func DeliveryStateKey(key []byte) []byte {
	return append([]byte("metadata/delivery/"), key...)
}

func ReadDeliveryState(db storage.DB, key []byte) (*DeliveryState, error) {
	data, closer, err := db.Get(DeliveryStateKey(key))
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close() //nolint:errcheck
	return UnmarshalDeliveryState(data)
}

func MarshalPrepareDeliveryCmd(key []byte, epoch uint64, recipients []string) ([]byte, error) {
	return marshalProtoPrepares([]DeliveryPrepare{{Key: key, Epoch: epoch, Recipients: recipients}}, ProtoPrepareDeliveryCmd)
}

func MarshalAckDeliveryBatchCmd(acks []DeliveryAck) ([]byte, error) {
	return marshalProtoAcks(acks)
}

// DeliveryBatch overlays uncommitted mutations so several prepares, ACKs, and
// deletes in one Dragonboat Update see each other, rather than stale DB reads.
// Call Flush before committing the storage batch to persist final receipts.
type DeliveryBatch struct {
	db      storage.DB
	batch   storage.Batch
	states  map[string]*DeliveryState
	present map[string]bool
	dirty   map[string]bool
}

func NewDeliveryBatch(db storage.DB, batch storage.Batch) *DeliveryBatch {
	return &DeliveryBatch{db: db, batch: batch, states: make(map[string]*DeliveryState), present: make(map[string]bool)}
}

func (b *DeliveryBatch) ObserveStore(key []byte) { b.present[string(key)] = true }

func (b *DeliveryBatch) Delete(key []byte) error {
	if err := b.batch.Delete(key); err != nil {
		return err
	}
	if err := b.batch.Delete(DeliveryStateKey(key)); err != nil {
		return err
	}
	b.present[string(key)] = false
	b.states[string(key)] = nil
	delete(b.dirty, string(key))
	return nil
}

func (b *DeliveryBatch) load(key []byte) (*DeliveryState, error) {
	if state, ok := b.states[string(key)]; ok {
		return state, nil
	}
	state, err := ReadDeliveryState(b.db, key)
	if err == nil {
		b.states[string(key)] = state
	}
	return state, err
}

func (b *DeliveryBatch) exists(key []byte) (bool, error) {
	if present, ok := b.present[string(key)]; ok {
		return present, nil
	}
	_, closer, err := b.db.Get(key)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, closer.Close()
}

func (b *DeliveryBatch) save(key []byte, state *DeliveryState) {
	b.states[string(key)] = state
	if b.dirty == nil {
		b.dirty = make(map[string]bool)
	}
	b.dirty[string(key)] = true
}

// Flush serializes each surviving receipt once, after all commands in this
// storage batch have updated the overlay. Deleted receipts require no encoding.
func (b *DeliveryBatch) Flush() error {
	for key := range b.dirty {
		data, err := marshalDeliveryState(b.states[key])
		if err != nil {
			return err
		}
		if err := b.batch.Set(DeliveryStateKey([]byte(key)), data); err != nil {
			return err
		}
		delete(b.dirty, key)
	}
	return nil
}

// Prepare applies a legacy single-key preparation without a command/result
// encoding. Its validation matches PrepareDeliveryCmd for log compatibility.
func (b *DeliveryBatch) Prepare(item DeliveryPrepare) (*DeliveryState, bool, error) {
	if len(item.Key) != 24 {
		return nil, false, fmt.Errorf("invalid delivery key length")
	}
	return b.prepare(item)
}

// PrepareBatch validates all inputs before changing the overlay.
func (b *DeliveryBatch) PrepareBatch(items []DeliveryPrepare) ([]DeliveryPrepared, [][]byte, error) {
	if err := ValidateDeliveryPrepares(items); err != nil {
		return nil, nil, err
	}
	return b.prepareBatch(items)
}

func (b *DeliveryBatch) prepareBatch(items []DeliveryPrepare) ([]DeliveryPrepared, [][]byte, error) {
	results := make([]DeliveryPrepared, 0, len(items))
	var deleted [][]byte
	for _, item := range items {
		state, removed, err := b.prepare(item)
		if err != nil {
			return nil, nil, err
		}
		results = append(results, DeliveryPrepared{Key: item.Key, State: state})
		if removed {
			deleted = append(deleted, item.Key)
		}
	}
	return results, deleted, nil
}

// Acknowledge applies typed ACKs to the same overlay used by Raft commands.
func (b *DeliveryBatch) Acknowledge(acks []DeliveryAck) ([][]byte, error) {
	var deleted [][]byte
	for _, ack := range acks {
		state, err := b.load(ack.Key)
		if err != nil {
			return nil, err
		}
		recipient := deliveryRecipient(ack.Recipient)
		if state == nil || !state.Needs(recipient) {
			continue
		}
		state.acknowledge(recipient)
		if state.complete() {
			if err := b.Delete(ack.Key); err != nil {
				return nil, err
			}
			deleted = append(deleted, ack.Key)
		} else {
			b.save(ack.Key, state)
		}
	}
	return deleted, nil
}

func (b *DeliveryBatch) Apply(cmd []byte) (statemachine.Result, [][]byte, error) {
	if len(cmd) == 0 {
		return statemachine.Result{}, nil, fmt.Errorf("empty delivery command")
	}
	switch CommandType(cmd[0]) {
	case PrepareDeliveryBatchCmd, ProtoPrepareDeliveryBatchCmd:
		items, err := UnmarshalPrepareDeliveryBatchCmd(cmd)
		if err != nil {
			return statemachine.Result{}, nil, err
		}
		results, deleted, err := b.prepareBatch(items)
		if err != nil {
			return statemachine.Result{}, nil, err
		}
		data, err := marshalPrepared(results, CommandType(cmd[0]) == PrepareDeliveryBatchCmd)
		return statemachine.Result{Value: uint64(len(results)), Data: data}, deleted, err
	case PrepareDeliveryCmd, ProtoPrepareDeliveryCmd:
		var prepare prepareDelivery
		if CommandType(cmd[0]) == ProtoPrepareDeliveryCmd {
			items, err := unmarshalProtoPrepares(cmd)
			if err != nil {
				return statemachine.Result{}, nil, err
			}
			if len(items) != 1 {
				return statemachine.Result{}, nil, fmt.Errorf("single prepare must contain one key")
			}
			prepare = items[0]
		} else if err := json.Unmarshal(cmd[1:], &prepare); err != nil {
			return statemachine.Result{}, nil, err
		}

		state, removed, err := b.Prepare(prepare)
		if err != nil {
			return statemachine.Result{}, nil, err
		}
		if removed {
			return statemachine.Result{}, [][]byte{prepare.Key}, nil
		}
		if state == nil {
			return statemachine.Result{}, nil, nil
		}
		data, err := marshalStateResult(state, CommandType(cmd[0]) == PrepareDeliveryCmd)
		return statemachine.Result{Value: 1, Data: data}, nil, err

	case AckDeliveryBatchCmd, ProtoAckDeliveryBatchCmd:
		var acks []DeliveryAck
		if CommandType(cmd[0]) == ProtoAckDeliveryBatchCmd {
			var err error
			acks, err = unmarshalProtoAcks(cmd[1:])
			if err != nil {
				return statemachine.Result{}, nil, err
			}
		} else if err := json.Unmarshal(cmd[1:], &acks); err != nil {
			return statemachine.Result{}, nil, err
		}

		deleted, err := b.Acknowledge(acks)
		return statemachine.Result{Value: uint64(len(deleted))}, deleted, err
	default:
		return statemachine.Result{}, nil, fmt.Errorf("invalid delivery command %d", cmd[0])
	}
}

func (b *DeliveryBatch) prepare(prepare DeliveryPrepare) (*DeliveryState, bool, error) {
	state, err := b.load(prepare.Key)
	if err != nil {
		return nil, false, err
	}
	exists, err := b.exists(prepare.Key)
	if err != nil || !exists {
		return nil, false, err
	}
	if state == nil {
		if len(prepare.Recipients) == 0 {
			return nil, false, nil
		}
		state = newDeliveryState(prepare.Epoch, prepare.Recipients)
	} else if prepare.Epoch > state.Epoch {
		active := make(map[string]bool, len(prepare.Recipients))
		for _, recipient := range prepare.Recipients {
			active[deliveryRecipient(recipient)] = true
		}
		// Fixed named interests and ACK bits survive re-preparation. Only
		// vanished ephemeral universal interests are completed on epoch change.
		for _, recipient := range state.Recipients {
			if strings.HasPrefix(recipient, "u:") && !active[recipient] {
				state.acknowledge(recipient)
			}
		}
		state.Epoch = prepare.Epoch
	}
	if state.complete() {
		return nil, true, b.Delete(prepare.Key)
	}
	b.save(prepare.Key, state)
	return state, false, nil
}
