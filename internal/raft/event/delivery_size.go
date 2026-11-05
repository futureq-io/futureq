package raft

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// Go's JSON implementations can emit a replacement rune as either UTF-8 or
// an escape. Determine its size once so byte limits match the active encoder.
var jsonReplacementSize = func() int {
	data, _ := json.Marshal("\xff")
	return len(data) - 2
}()

// DeliveryPrepareSize is the exact encoding/json size of an item, without
// allocating encoded bytes. Keep this in step with DeliveryPrepare's wire shape.
func DeliveryPrepareSize(item DeliveryPrepare) int {
	size := len(`{"key":,"epoch":,"recipients":}`)
	if item.Key == nil {
		size += 4 // null
	} else {
		size += 2 + base64.StdEncoding.EncodedLen(len(item.Key))
	}
	size++ // at least one decimal digit
	for epoch := item.Epoch; epoch >= 10; epoch /= 10 {
		size++
	}
	if item.Recipients == nil {
		return size + 4 // null
	}
	size += 2 // []
	for i, recipient := range item.Recipients {
		if i > 0 {
			size++
		}
		size += jsonStringSize(recipient)
	}
	return size
}

// ValidateDeliveryPrepares checks the complete request, including its encoded
// byte limit, even when every manifest is already present. It never encodes it.
func ValidateDeliveryPrepares(items []DeliveryPrepare) error {
	if err := validatePrepares(items); err != nil {
		return err
	}
	size := 3 // command tag and []
	for i, item := range items {
		if i > 0 {
			size++
		}
		size += DeliveryPrepareSize(item)
		if size > MaxDeliveryPrepareBytes {
			return fmt.Errorf("delivery prepare batch exceeds %d bytes", MaxDeliveryPrepareBytes)
		}
	}
	return nil
}

func jsonStringSize(s string) int {
	size := 2 // quotes
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"', '\\', '\b', '\f', '\n', '\r', '\t':
				size += 2
			case '<', '>', '&':
				size += 6 // encoding/json's default HTML escaping
			default:
				if c < 0x20 {
					size += 6
				} else {
					size++
				}
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			size += jsonReplacementSize
		} else if r == '\u2028' || r == '\u2029' {
			size += 6
		} else {
			size += n
		}
		i += n
	}
	return size
}

// Match the former JSON command round trip for malformed UTF-8, replacing
// each invalid byte separately. Valid recipient names retain their storage.
func deliveryRecipient(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	runes := []rune(s)
	return string(runes)
}
