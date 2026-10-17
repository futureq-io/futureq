package config

import "fmt"

const (
	DefaultPayloadCacheSlots = 4096
	MinPayloadCacheSlots     = 64
	MaxPayloadCacheSlots     = 1 << 20
)

// ValidatePayloadCacheSlots keeps tracker memory bounded and permits masking
// instead of division when mapping event keys to slots.
func ValidatePayloadCacheSlots(slots int) error {
	if slots < MinPayloadCacheSlots || slots > MaxPayloadCacheSlots || slots&(slots-1) != 0 {
		return fmt.Errorf("storage.payloadCacheSlots must be a power of two from %d to %d", MinPayloadCacheSlots, MaxPayloadCacheSlots)
	}
	return nil
}
