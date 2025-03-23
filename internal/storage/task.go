package storage

import (
	"time"
)

type task struct {
	payload []byte
	at      time.Time
}

func (t task) Payload() []byte {
	return t.payload
}
