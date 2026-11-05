package raft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/bits"
	"sort"
	"unicode/utf8"

	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

var receiptPrefix = []byte{'F', 'Q', 'R', 1}
var preparedPrefix = []byte{'F', 'Q', 'P', 1}
var deliveryUnmarshal = proto.UnmarshalOptions{DiscardUnknown: true, RecursionLimit: 16}

type legacyDeliveryState struct {
	Epoch      uint64          `json:"epoch"`
	Recipients map[string]bool `json:"recipients"`
}

func newDeliveryState(epoch uint64, recipients []string) *DeliveryState {
	names := make([]string, len(recipients))
	for i, r := range recipients {
		names[i] = deliveryRecipient(r)
	}
	sort.Strings(names)
	// Legacy single-key commands permitted duplicate recipients.
	unique := names[:0]
	for _, name := range names {
		if len(unique) == 0 || unique[len(unique)-1] != name {
			unique = append(unique, name)
		}
	}
	return &DeliveryState{Epoch: epoch, Recipients: unique, Acknowledged: make([]byte, (len(unique)+7)/8), remaining: len(unique)}
}

// MarshalJSON is exclusively for legacy log results and migration tests.
func (s *DeliveryState) MarshalJSON() ([]byte, error) {
	legacy := legacyDeliveryState{Epoch: s.Epoch, Recipients: make(map[string]bool, len(s.Recipients))}
	for _, r := range s.Recipients {
		legacy.Recipients[r] = !s.Needs(r)
	}
	return json.Marshal(legacy)
}
func (s *DeliveryState) UnmarshalJSON(data []byte) error {
	var legacy legacyDeliveryState
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	names := make([]string, 0, len(legacy.Recipients))
	for r := range legacy.Recipients {
		names = append(names, r)
	}
	*s = *newDeliveryState(legacy.Epoch, names)
	for r, acked := range legacy.Recipients {
		if acked {
			s.acknowledge(r)
		}
	}
	return nil
}
func receiptProto(s *DeliveryState) *deliverypb.DeliveryReceipt {
	if s == nil {
		return nil
	}
	return &deliverypb.DeliveryReceipt{Epoch: s.Epoch, Recipients: s.Recipients, Acknowledged: s.Acknowledged}
}
func receiptState(p *deliverypb.DeliveryReceipt) (*DeliveryState, error) {
	if p == nil {
		return nil, nil
	}
	if len(p.Acknowledged) != (len(p.Recipients)+7)/8 {
		return nil, fmt.Errorf("invalid receipt bitmap length")
	}
	for i, r := range p.Recipients {
		if i > 0 && r <= p.Recipients[i-1] {
			return nil, fmt.Errorf("receipt recipients are not unique and sorted")
		}
	}
	remaining := len(p.Recipients)
	for _, b := range p.Acknowledged {
		remaining -= bits.OnesCount8(b)
	}
	if n := len(p.Recipients) % 8; n != 0 && p.Acknowledged[len(p.Acknowledged)-1]>>uint(n) != 0 {
		return nil, fmt.Errorf("invalid receipt bitmap padding")
	}
	return &DeliveryState{Epoch: p.Epoch, Recipients: p.Recipients, Acknowledged: p.Acknowledged, remaining: remaining}, nil
}
func marshalDeliveryState(s *DeliveryState) ([]byte, error) {
	return proto.MarshalOptions{}.MarshalAppend(append([]byte(nil), receiptPrefix...), receiptProto(s))
}
func marshalStateResult(s *DeliveryState, legacy bool) ([]byte, error) {
	if legacy {
		return json.Marshal(s)
	}
	return marshalDeliveryState(s)
}

// UnmarshalDeliveryState reads both persisted formats. New writes use Protobuf.
func UnmarshalDeliveryState(data []byte) (*DeliveryState, error) {
	if bytes.HasPrefix(data, receiptPrefix) {
		return decodeStoredReceipt(data[len(receiptPrefix):])
	}
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return nil, fmt.Errorf("unknown delivery receipt format")
	}
	var state DeliveryState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// DeliveryStateEpoch avoids allocating the recipient roster during scans.
func DeliveryStateEpoch(data []byte) (uint64, error) {
	if bytes.HasPrefix(data, receiptPrefix) {
		var p deliverypb.DeliveryReceiptEpoch
		err := deliveryUnmarshal.Unmarshal(data[len(receiptPrefix):], &p)
		return p.Epoch, err
	}
	state, err := UnmarshalDeliveryState(data)
	if err != nil {
		return 0, err
	}
	return state.Epoch, nil
}
func marshalPrepared(items []DeliveryPrepared, legacy bool) ([]byte, error) {
	if legacy {
		return json.Marshal(items)
	}
	p := &deliverypb.DeliveryPreparedBatch{Items: make([]*deliverypb.DeliveryPrepared, len(items))}
	for i, item := range items {
		p.Items[i] = &deliverypb.DeliveryPrepared{Key: item.Key, State: receiptProto(item.State)}
	}
	return proto.MarshalOptions{}.MarshalAppend(append([]byte(nil), preparedPrefix...), p)
}
func UnmarshalDeliveryPrepared(data []byte) ([]DeliveryPrepared, error) {
	if !bytes.HasPrefix(data, preparedPrefix) {
		var items []DeliveryPrepared
		err := json.Unmarshal(data, &items)
		return items, err
	}
	var p deliverypb.DeliveryPreparedBatch
	if err := deliveryUnmarshal.Unmarshal(data[len(preparedPrefix):], &p); err != nil {
		return nil, err
	}
	items := make([]DeliveryPrepared, len(p.Items))
	for i, item := range p.Items {
		state, err := receiptState(item.State)
		if err != nil {
			return nil, err
		}
		items[i] = DeliveryPrepared{Key: item.Key, State: state}
	}
	return items, nil
}

func marshalProtoPrepares(items []DeliveryPrepare, tag CommandType) ([]byte, error) {
	if err := ValidateDeliveryPrepares(items); err != nil {
		return nil, err
	}
	p := &deliverypb.DeliveryPrepareBatch{}
	groups := make(map[prepareViewKey]*deliverypb.DeliveryPrepareGroup)
	for _, item := range items {
		key := prepareView(item)
		group := groups[key]
		if group == nil {
			group = &deliverypb.DeliveryPrepareGroup{Epoch: item.Epoch, Recipients: normalizedRecipients(item.Recipients)}
			groups[key] = group
			p.Groups = append(p.Groups, group)
		}
		group.Keys = append(group.Keys, item.Key)
	}
	return proto.MarshalOptions{}.MarshalAppend([]byte{byte(tag)}, p)
}
func normalizedRecipients(names []string) []string {
	for _, r := range names {
		if deliveryRecipient(r) != r {
			out := make([]string, len(names))
			for i, r := range names {
				out[i] = deliveryRecipient(r)
			}
			return out
		}
	}
	return names
}

// Count bounded repeated fields before the generated decoder allocates them.
func preflightPrepare(data []byte) error {
	groups, keys := 0, 0
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return protowire.ParseError(n)
		}
		data = data[n:]
		if num == 1 {
			if typ != protowire.BytesType {
				return fmt.Errorf("invalid prepare group wire type")
			}
			group, m := protowire.ConsumeBytes(data)
			if m < 0 {
				return protowire.ParseError(m)
			}
			data = data[m:]
			groups++
			if groups > MaxDeliveryPrepareItems {
				return fmt.Errorf("too many prepare groups")
			}
			recipients := 0
			for len(group) > 0 {
				field, wire, k := protowire.ConsumeTag(group)
				if k < 0 {
					return protowire.ParseError(k)
				}
				group = group[k:]
				if field == 2 || field == 3 {
					if wire != protowire.BytesType {
						return fmt.Errorf("invalid prepare field wire type")
					}
					value, m := protowire.ConsumeBytes(group)
					if m < 0 {
						return protowire.ParseError(m)
					}
					group = group[m:]
					if field == 2 {
						recipients++
						if recipients > MaxDeliveryRecipients {
							return fmt.Errorf("too many delivery recipients")
						}
					} else {
						keys++
						if len(value) != 24 || keys > MaxDeliveryPrepareItems {
							return fmt.Errorf("invalid delivery prepare keys")
						}
					}
				} else {
					if field == 1 && wire != protowire.VarintType {
						return fmt.Errorf("invalid epoch wire type")
					}
					m := protowire.ConsumeFieldValue(field, wire, group)
					if m < 0 {
						return protowire.ParseError(m)
					}
					group = group[m:]
				}
			}
		} else {
			m := protowire.ConsumeFieldValue(num, typ, data)
			if m < 0 {
				return protowire.ParseError(m)
			}
			data = data[m:]
		}
	}
	return nil
}
func unmarshalProtoPrepares(cmd []byte) ([]DeliveryPrepare, error) {
	if len(cmd) < 1 || len(cmd) > MaxDeliveryPrepareBytes {
		return nil, fmt.Errorf("invalid prepare command size")
	}
	if err := preflightPrepare(cmd[1:]); err != nil {
		return nil, err
	}
	var p deliverypb.DeliveryPrepareBatch
	if err := deliveryUnmarshal.Unmarshal(cmd[1:], &p); err != nil {
		return nil, err
	}
	items := make([]DeliveryPrepare, 0, MaxDeliveryPrepareItems)
	for _, g := range p.Groups {
		if len(g.Keys) == 0 {
			return nil, fmt.Errorf("empty prepare group")
		}
		for _, key := range g.Keys {
			items = append(items, DeliveryPrepare{Key: key, Epoch: g.Epoch, Recipients: g.Recipients})
		}
	}
	return items, validatePrepares(items)
}

// Append the schema's wire fields directly to avoid one Go wrapper per ACK.
func marshalProtoAcks(acks []DeliveryAck) ([]byte, error) {
	size := 1
	for _, a := range acks {
		n := 0
		if len(a.Key) > 0 {
			n += 1 + protowire.SizeBytes(len(a.Key))
		}
		r := deliveryRecipient(a.Recipient)
		if r != "" {
			n += 1 + protowire.SizeBytes(len(r))
		}
		size += 1 + protowire.SizeBytes(n)
	}
	out := make([]byte, 1, size)
	out[0] = byte(ProtoAckDeliveryBatchCmd)
	for _, a := range acks {
		r := deliveryRecipient(a.Recipient)
		n := 0
		if len(a.Key) > 0 {
			n += 1 + protowire.SizeBytes(len(a.Key))
		}
		if r != "" {
			n += 1 + protowire.SizeBytes(len(r))
		}
		out = protowire.AppendTag(out, 1, protowire.BytesType)
		out = protowire.AppendVarint(out, uint64(n))
		if len(a.Key) > 0 {
			out = protowire.AppendTag(out, 1, protowire.BytesType)
			out = protowire.AppendBytes(out, a.Key)
		}
		if r != "" {
			out = protowire.AppendTag(out, 2, protowire.BytesType)
			out = protowire.AppendString(out, r)
		}
	}
	return out, nil
}
func unmarshalProtoAcks(data []byte) ([]DeliveryAck, error) {
	original := data
	count := 0
	for len(data) > 0 {
		field, wire, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		data = data[n:]
		if field != 1 || wire != protowire.BytesType {
			return unmarshalProtoAcksGeneral(original)
		}
		_, m := protowire.ConsumeBytes(data)
		if m < 0 {
			return nil, protowire.ParseError(m)
		}
		data = data[m:]
		count++
	}
	acks := make([]DeliveryAck, count)
	data = original
	for i := range acks {
		_, _, n := protowire.ConsumeTag(data)
		body, m := protowire.ConsumeBytes(data[n:])
		data = data[n+m:]
		for len(body) > 0 {
			field, wire, k := protowire.ConsumeTag(body)
			if k < 0 {
				return nil, protowire.ParseError(k)
			}
			body = body[k:]
			if (field != 1 && field != 2) || wire != protowire.BytesType {
				return unmarshalProtoAcksGeneral(original)
			}
			value, l := protowire.ConsumeBytes(body)
			if l < 0 {
				return nil, protowire.ParseError(l)
			}
			body = body[l:]
			if field == 1 {
				acks[i].Key = value
			} else {
				if !utf8.Valid(value) {
					return nil, fmt.Errorf("invalid ACK recipient UTF-8")
				}
				acks[i].Recipient = string(value)
			}
		}
	}
	return acks, nil
}

// Unknown fields retain the generated decoder's evolution and depth semantics.
func unmarshalProtoAcksGeneral(data []byte) ([]DeliveryAck, error) {
	var p deliverypb.DeliveryAckBatch
	if err := deliveryUnmarshal.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	acks := make([]DeliveryAck, len(p.Acks))
	for i, a := range p.Acks {
		acks[i] = DeliveryAck{Key: a.Key, Recipient: a.Recipient}
	}
	return acks, nil
}

// Match the previous JSON command normalization for invalid UTF-8 names.
func deliveryRecipient(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return string([]rune(s))
}
