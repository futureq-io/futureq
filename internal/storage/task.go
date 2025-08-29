package storage

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"time"
)

type Task struct {
	Payload []byte
	ID      string
	At      time.Time
}

func (t *Task) String() string {
	return fmt.Sprintf("%s-%s-%s", t.ID, string(t.Payload), t.At.String())
}

type Tasks []Task

func (t Tasks) toGOB() ([]byte, error) {
	var buffer bytes.Buffer

	err := gob.NewEncoder(&buffer).
		Encode(t)

	return buffer.Bytes(), err
}

func init() {
	gob.Register(time.Time{})
	gob.Register(Tasks{})
}

func FromGOB(v []byte) (Tasks, error) {
	var t Tasks

	err := gob.NewDecoder(bytes.NewReader(v)).
		Decode(&t)

	return t, err
}
