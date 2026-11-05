package raft

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

// The scheduler passes one immutable recipient slice for an entire topic view.
// Identity sharing avoids hashing/copying names for every candidate. Equal
// slices with different backing arrays remain valid independent wire groups.
type prepareViewKey struct {
	epoch uint64
	first *string
	count int
}

func prepareView(item DeliveryPrepare) prepareViewKey {
	key := prepareViewKey{epoch: item.Epoch, count: len(item.Recipients)}
	if len(item.Recipients) > 0 {
		key.first = &item.Recipients[0]
	}
	return key
}
func prepareViewSize(item DeliveryPrepare) int {
	size := 0
	if item.Epoch != 0 {
		size += 1 + protowire.SizeVarint(item.Epoch)
	}
	for _, r := range item.Recipients {
		size += 1 + protowire.SizeBytes(len(deliveryRecipient(r)))
	}
	return size
}

// DeliveryPrepareSizer accounts for shared recipient views and changing varint
// lengths without encoding bytes. The zero value includes the command tag.
type DeliveryPrepareSizer struct {
	groups map[prepareViewKey]int
	size   int
}

func (s *DeliveryPrepareSizer) next(item DeliveryPrepare) (prepareViewKey, int, int) {
	key := prepareView(item)
	old, exists := s.groups[key]
	body := old
	if !exists {
		body = prepareViewSize(item)
	}
	body += 1 + protowire.SizeBytes(len(item.Key))
	total := s.size
	if total == 0 {
		total = 1
	}
	total += 1 + protowire.SizeBytes(body)
	if exists {
		total -= 1 + protowire.SizeBytes(old)
	}
	return key, body, total
}
func (s *DeliveryPrepareSizer) SizeWith(item DeliveryPrepare) int {
	_, _, size := s.next(item)
	return size
}
func (s *DeliveryPrepareSizer) Add(item DeliveryPrepare) int {
	key, body, size := s.next(item)
	if s.groups == nil {
		s.groups = make(map[prepareViewKey]int)
	}
	s.groups[key] = body
	s.size = size
	return size
}
func DeliveryPrepareSize(item DeliveryPrepare) int { var s DeliveryPrepareSizer; return s.Add(item) }
func ValidateDeliveryPrepares(items []DeliveryPrepare) error {
	if err := validatePrepares(items); err != nil {
		return err
	}
	var size DeliveryPrepareSizer
	for _, item := range items {
		if size.Add(item) > MaxDeliveryPrepareBytes {
			return fmt.Errorf("delivery prepare batch exceeds %d bytes", MaxDeliveryPrepareBytes)
		}
	}
	return nil
}
