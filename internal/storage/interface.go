package storage

import (
	"time"
)

type TaskStorage interface {
	InitiatePersistence() error
	Add(payload []byte, at time.Time)
	PopLesserThan(v time.Time) []task
	LesserThan(v time.Time) []task
}
