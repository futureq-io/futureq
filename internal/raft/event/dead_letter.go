package raft

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/futureq-io/futureq/internal/storage"
	"github.com/futureq-io/futureq/pkg/utils"
	pb "github.com/futureq-io/protocol/proto/go"
	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	storagepb "github.com/futureq-io/protocol/proto/go/storage"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

var ErrDeadLetterPageTooSmall = errors.New("gRPC send limit cannot fit one dead letter summary and its page token")

const (
	deadLetterEntries   = "metadata/dead-letter/entries/"
	deadLetterPayload   = "metadata/dead-letter/payload/"
	deadLetterRefs      = "metadata/dead-letter/refs/"
	deadLetterPageBytes = 32 << 10
)

func ValidateDeadLetterID(topic string, id []byte) error {
	if err := ValidateTopicName(topic); err != nil {
		return err
	}
	if len(id) <= utils.EventKeySize || !validRetryRecipient(string(id[utils.EventKeySize:])) ||
		binary.BigEndian.Uint64(id) != utils.TopicHash(topic) {
		return fmt.Errorf("invalid dead letter ID for topic")
	}
	return nil
}

func deadLetterEntryPrefix(topic string) []byte {
	return []byte(deadLetterEntries + topic + "\x00")
}

func (b *DeliveryBatch) archiveDeadLetter(message *deliverypb.StoredMessageMetadata, target *deliverypb.DeliveryAttemptTarget, attempts uint64, nowMs int64, reason pb.DeadLetterReason) (uint64, error) {
	summary := &pb.DeadLetterSummary{
		Id:    append(bytes.Clone(target.Key), target.Recipient...),
		Topic: message.Topic, Generation: target.Recipient, Attempts: attempts, Reason: reason,
		DeadLetteredAtUnixMs: nowMs, EnqueuedAtUnixMs: message.EnqueuedAtUnixMs,
	}
	if strings.HasPrefix(target.Recipient, subscriptionRecipientPrefix) {
		subscription, err := b.subscriptionRead([]byte(subscriptionGenerationPrefix + target.Recipient))
		if err != nil || !subscription.GetActive() {
			return 0, err
		}
		summary.GroupId, summary.SubscriberId = subscription.GroupId, subscription.SubscriberId
	} else if strings.HasPrefix(target.Recipient, "g:") {
		summary.GroupId = target.Recipient[2:]
	} else {
		summary.SubscriberId = target.Recipient[2:]
	}
	refKey := append([]byte(deadLetterRefs), target.Key...)
	refs, err := b.deadLetterRefCount(refKey)
	if err != nil {
		return 0, err
	}
	if refs == 0 {
		// Fan-out shares a single immutable archived payload across failures.
		data, closer, err := b.db.Get(target.Key)
		if err != nil {
			return 0, err
		}
		defer closer.Close() //nolint:errcheck
		if err := b.batch.Set(append([]byte(deadLetterPayload), target.Key...), data); err != nil {
			return 0, err
		}
	}
	data, err := proto.Marshal(summary)
	if err != nil {
		return 0, err
	}
	entryKey := append(deadLetterEntryPrefix(message.Topic), summary.Id...)
	if err := b.batch.Set(entryKey, data); err != nil {
		return 0, err
	}
	if b.archiveEntries == nil {
		b.archiveEntries = make(map[string]bool)
	}
	b.archiveEntries[string(entryKey)] = true
	return 1, b.writeDeadLetterRefCount(refKey, refs+1)
}

func (b *DeliveryBatch) deadLetterRefCount(key []byte) (uint32, error) {
	if count, ok := b.archiveRefs[string(key)]; ok {
		return count, nil
	}
	data, closer, err := b.db.Get(key)
	if errors.Is(err, storage.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer closer.Close() //nolint:errcheck
	if len(data) != 4 || binary.BigEndian.Uint32(data) > MaxDeliveryRecipients {
		return 0, fmt.Errorf("invalid dead letter reference count")
	}
	return binary.BigEndian.Uint32(data), nil
}

func (b *DeliveryBatch) writeDeadLetterRefCount(key []byte, count uint32) error {
	if b.archiveRefs == nil {
		b.archiveRefs = make(map[string]uint32)
	}
	b.archiveRefs[string(key)] = count
	if count == 0 {
		return b.batch.Delete(key)
	}
	return b.batch.Set(key, binary.BigEndian.AppendUint32(nil, count))
}

func (b *DeliveryBatch) deleteDeadLetter(topic string, id []byte) error {
	entryKey := append(deadLetterEntryPrefix(topic), id...)
	present, known := b.archiveEntries[string(entryKey)]
	if !known {
		_, closer, err := b.db.Get(entryKey)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := closer.Close(); err != nil {
			return err
		}
		present = true
	}
	if !present {
		return nil
	}
	key := id[:utils.EventKeySize]
	refKey := append([]byte(deadLetterRefs), key...)
	refs, err := b.deadLetterRefCount(refKey)
	if err != nil {
		return err
	}
	if refs == 0 {
		return fmt.Errorf("missing dead letter payload reference")
	}
	if err := b.batch.Delete(entryKey); err != nil {
		return err
	}
	if refs == 1 {
		if err := b.batch.Delete(append([]byte(deadLetterPayload), key...)); err != nil {
			return err
		}
	}
	if b.archiveEntries == nil {
		b.archiveEntries = make(map[string]bool)
	}
	b.archiveEntries[string(entryKey)] = false
	return b.writeDeadLetterRefCount(refKey, refs-1)
}

type DeadLetterQuery struct {
	Topic    string
	ID       []byte // exact lookup when set
	After    []byte // exclusive cursor when listing
	Limit    int
	MaxBytes int
}

type DeadLetterResult struct {
	Entry *pb.DeadLetter
	Page  *pb.ListDeadLettersResponse
}

func (q DeadLetterQuery) readForLookup(db storage.DB) (any, error) {
	return q.Read(db)
}

func (q DeadLetterQuery) Read(db storage.DB) (DeadLetterResult, error) {
	if len(q.ID) != 0 {
		entry, err := q.get(db)
		return DeadLetterResult{Entry: entry}, err
	}
	page, err := q.list(db)
	return DeadLetterResult{Page: page}, err
}

func (q DeadLetterQuery) get(db storage.DB) (*pb.DeadLetter, error) {
	if err := ValidateDeadLetterID(q.Topic, q.ID); err != nil {
		return nil, err
	}
	// Both reads use one snapshot so concurrent deletion cannot expose a
	// summary without its payload.
	iter, err := db.NewIter(nil)
	if err != nil {
		return nil, err
	}
	defer iter.Close() //nolint:errcheck
	entryKey := append(deadLetterEntryPrefix(q.Topic), q.ID...)
	if !iter.SeekGE(entryKey) || !bytes.Equal(iter.Key(), entryKey) {
		return nil, iter.Error()
	}
	summary := new(pb.DeadLetterSummary)
	if err := deliveryUnmarshal.Unmarshal(iter.Value(), summary); err != nil {
		return nil, err
	}
	payloadKey := append([]byte(deadLetterPayload), q.ID[:utils.EventKeySize]...)
	if !iter.SeekGE(payloadKey) || !bytes.Equal(iter.Key(), payloadKey) {
		return nil, errors.Join(fmt.Errorf("dead letter payload missing"), iter.Error())
	}
	message := new(storagepb.StoredMessage)
	if err := deliveryUnmarshal.Unmarshal(iter.Value(), message); err != nil {
		return nil, err
	}
	return &pb.DeadLetter{Summary: summary, Message: &pb.PublishMessage{
		Topic: message.Topic, Payload: message.Payload, DelayMs: message.DelayMs,
		TtlMs: message.TtlMs, Indexes: message.Indexes, MaxRetries: message.MaxRetries,
	}}, nil
}

func (q DeadLetterQuery) list(db storage.DB) (*pb.ListDeadLettersResponse, error) {
	if err := ValidateTopicName(q.Topic); err != nil {
		return nil, err
	}
	if q.Limit < 1 || q.Limit > 256 {
		return nil, fmt.Errorf("dead letter page size must be 1..256")
	}
	prefix := deadLetterEntryPrefix(q.Topic)
	lower := prefix
	if len(q.After) > 0 {
		if err := ValidateDeadLetterID(q.Topic, q.After); err != nil {
			return nil, err
		}
		lower = append(append(bytes.Clone(prefix), q.After...), 0)
	}
	upper := bytes.Clone(prefix)
	upper[len(upper)-1] = 1
	iter, err := db.NewIter(&storage.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	defer iter.Close() //nolint:errcheck
	page := new(pb.ListDeadLettersResponse)
	budget := deadLetterPageBytes
	if q.MaxBytes > 0 {
		budget = min(budget, q.MaxBytes)
	}
	var size int
	for ok := iter.First(); ok; {
		summary := new(pb.DeadLetterSummary)
		if err := deliveryUnmarshal.Unmarshal(iter.Value(), summary); err != nil {
			return nil, err
		}
		ok = iter.Next()
		entrySize := protowire.SizeTag(1) + protowire.SizeBytes(proto.Size(summary))
		tokenSize := 0
		if ok {
			tokenSize = protowire.SizeTag(2) + protowire.SizeBytes(len(summary.Id))
		}
		if size+entrySize+tokenSize > budget {
			if len(page.Entries) == 0 {
				return nil, ErrDeadLetterPageTooSmall
			}
			page.NextPageToken = page.Entries[len(page.Entries)-1].Id
			break
		}
		page.Entries = append(page.Entries, summary)
		size += entrySize
		if ok && len(page.Entries) == q.Limit {
			page.NextPageToken = summary.Id
			break
		}
	}
	return page, iter.Error()
}
