package ticker

import (
	"time"

	"github.com/futureq-io/futureq/internal/q"
	"github.com/futureq-io/futureq/internal/storage"
)

type Ticker interface {
	Tick()
}

type ticker struct {
	strg storage.TaskStorage
	q    q.Q
}

func NewTicker(strg storage.TaskStorage, q q.Q) Ticker {
	return &ticker{
		strg: strg,
		q:    q,
	}
}

func (t *ticker) Tick() {
	ticker := time.NewTicker(1 * time.Second)
	for tickedAt := range ticker.C {
		result := t.strg.PopLesserThan(tickedAt)
		for _, re := range result {
			t.q.Publish(re.Payload())
		}
	}
}
