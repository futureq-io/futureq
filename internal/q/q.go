package q

import (
	"github.com/futureq-io/futureq/internal/storage"
)

type Q interface {
	Connect() error
	Consume(storage storage.TaskStorage) error
	Publish(payload []byte)
	Close()
}
