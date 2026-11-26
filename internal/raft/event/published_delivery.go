package raft

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/futureq-io/futureq/pkg/utils"
)

// MarshalPublishReceipt freezes destinations before the payload is committed.
func MarshalPublishReceipt(epoch uint64, recipients []string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("message has no registered destinations")
	}
	if err := validatePrepares([]DeliveryPrepare{{Key: make([]byte, utils.EventKeySize), Epoch: epoch, Recipients: recipients}}); err != nil {
		return nil, err
	}
	s := newDeliveryState(epoch, recipients)
	s.PublishBound = true
	return marshalDeliveryState(s)
}

func validatePublishReceipt(data []byte) error {
	s, err := UnmarshalDeliveryState(data)
	if err != nil {
		return err
	}
	if !s.PublishBound || len(s.Recipients) == 0 || len(s.Recipients) > MaxDeliveryRecipients || s.remaining != len(s.Recipients) {
		return fmt.Errorf("invalid publish receipt")
	}
	for _, name := range s.Recipients {
		if len(name) <= 2 || (!strings.HasPrefix(name, "g:") && !strings.HasPrefix(name, "u:")) || !utf8.ValidString(name) {
			return fmt.Errorf("invalid publish recipient")
		}
	}
	return nil
}
