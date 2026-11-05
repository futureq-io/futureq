package raft

import (
	"fmt"
	"unicode/utf8"

	deliverypb "github.com/futureq-io/protocol/proto/go/delivery"
	"google.golang.org/protobuf/encoding/protowire"
)

// decodeStoredReceipt keeps an owned string backing the entire roster, avoiding
// one string allocation per recipient and repeated slice growth. It reads the
// schema's standard Protobuf wire format; no storage-owned bytes escape Get.
func decodeStoredReceipt(data []byte) (*DeliveryState, error) {
	count := 0
	var epoch uint64
	var acked []byte
	for pos := 0; pos < len(data); {
		field, wire, n := protowire.ConsumeTag(data[pos:])
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		pos += n
		switch {
		case field == 1 && wire == protowire.VarintType:
			value, m := protowire.ConsumeVarint(data[pos:])
			if m < 0 {
				return nil, protowire.ParseError(m)
			}
			epoch = value
			pos += m
		case (field == 2 || field == 3) && wire == protowire.BytesType:
			value, m := protowire.ConsumeBytes(data[pos:])
			if m < 0 {
				return nil, protowire.ParseError(m)
			}
			if field == 2 {
				if !utf8.Valid(value) {
					return nil, fmt.Errorf("invalid receipt UTF-8")
				}
				count++
			} else {
				acked = value
			}
			pos += m
		default:
			// Preserve the generated decoder's unknown-field, wire-type, and
			// recursion semantics on schema evolution or noncanonical input.
			var p deliverypb.DeliveryReceipt
			if err := deliveryUnmarshal.Unmarshal(data, &p); err != nil {
				return nil, err
			}
			return receiptState(&p)
		}
	}
	if len(acked) != (count+7)/8 {
		return nil, fmt.Errorf("invalid receipt bitmap length")
	}
	owned := string(data)
	names := make([]string, count)
	i := 0
	for pos := 0; pos < len(data); {
		field, wire, n := protowire.ConsumeTag(data[pos:])
		pos += n
		if field == 2 && wire == protowire.BytesType {
			value, m := protowire.ConsumeBytes(data[pos:])
			start := pos + m - len(value)
			names[i] = owned[start : start+len(value)]
			i++
			pos += m
		} else {
			pos += protowire.ConsumeFieldValue(field, wire, data[pos:])
		}
	}
	return receiptState(&deliverypb.DeliveryReceipt{Epoch: epoch, Recipients: names, Acknowledged: append([]byte(nil), acked...)})
}
