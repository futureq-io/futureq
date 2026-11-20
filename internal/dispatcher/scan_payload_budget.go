package dispatcher

import (
	"context"
	"sync/atomic"
)

// Charge encoded payloads before decoding. An oversized message can claim
// an untouched pass for itself.
type scanPayloadBudget struct {
	limit     int64
	remaining atomic.Int64
}

func (b *scanPayloadBudget) take(size int) bool {
	cost := int64(size)
	for remaining := b.remaining.Load(); remaining > 0; remaining = b.remaining.Load() {
		charge := cost
		if cost > remaining {
			if remaining != b.limit {
				return false
			}
			charge = remaining
		}
		if b.remaining.CompareAndSwap(remaining, remaining-charge) {
			return true
		}
	}
	return false
}

// The rotating lead lane gets first access to the budget. Other lanes can
// open and position their iterators in parallel before admitting payloads.
type scanPayloadAdmission struct {
	budget  *scanPayloadBudget
	ctx     context.Context
	wait    <-chan struct{}
	release func()
}

func (a scanPayloadAdmission) full() bool {
	return a.budget != nil && a.budget.remaining.Load() == 0
}

func (a *scanPayloadAdmission) awaitTurn() bool {
	if a.wait != nil {
		select {
		case <-a.ctx.Done():
			return false
		case <-a.wait:
			a.wait = nil
		}
	}
	return true
}

func (a *scanPayloadAdmission) reserve(size int) bool {
	if a.budget == nil {
		return true
	}
	if !a.awaitTurn() {
		return false
	}
	accepted := a.budget.take(size)
	if a.release != nil {
		a.release()
	}
	return accepted
}
