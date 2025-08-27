package storage

import (
	"time"
)

type task struct {
	payload []byte
	id      string
	at      time.Time
}

func (t task) Payload() []byte {
	return t.payload
}
