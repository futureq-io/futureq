package storage

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/google/uuid"

	"github.com/futureq-io/futureq/internal/config"
)

type memoryArray struct {
	tasks Tasks
	lock  *sync.RWMutex

	cfg config.Persistence
	db  *pebble.DB
}

func NewMemoryArray(cfg config.Persistence) TaskStorage {
	return &memoryArray{
		//Tasks: make([]Task, 0),
		lock: new(sync.RWMutex),
		cfg:  cfg,
	}
}

func (s *memoryArray) InitiatePersistence() error {
	var err error

	s.db, err = pebble.Open(s.cfg.Path, &pebble.Options{})
	if err != nil {
		return fmt.Errorf("could not open database: %v", err)
	}

	return s.loadTasksFromDisk()
}

func (s *memoryArray) Add(payload []byte, at time.Time) {
	id := uuid.New().String()
	t := Task{ID: id, At: at}

	s.lock.Lock()
	defer s.lock.Unlock()

	s.tasks = append(s.tasks, t)

	for i := len(s.tasks) - 1; i > 0; i-- {
		if s.tasks[i].At.Before(s.tasks[i-1].At) {
			s.tasks[i], s.tasks[i-1] = s.tasks[i-1], s.tasks[i]
		}
	}

	err := s.saveTasksOnDisk()
	if err != nil {
		fmt.Println(err)
	}

	err = s.db.Set([]byte(id), payload, pebble.Sync)
	if err != nil {
		fmt.Println(err)
	}

}

func (s *memoryArray) PopLesserThan(v time.Time) []Task {
	res, i := s.lesserThan(v)
	s.popFromI(i)

	return res
}

func (s *memoryArray) LesserThan(v time.Time) []Task {
	res, _ := s.lesserThan(v)

	return res
}

func (s *memoryArray) lesserThan(v time.Time) ([]Task, int) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	result := make([]Task, 0)

	var i = 0

	for ; i < len(s.tasks); i++ {
		if s.tasks[i].At.After(v) {
			break
		}

		result = append(result, s.tasks[i])
	}

	for i = 0; i < len(result); i++ {
		payload, closer, _ := s.db.Get([]byte(result[i].ID))
		_ = closer.Close()

		_ = s.db.Delete([]byte(result[i].ID), nil)

		result[i].Payload = payload
	}

	return result, i
}

func (s *memoryArray) popFromI(i int) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.tasks = s.tasks[i:]

	err := s.saveTasksOnDisk()
	if err != nil {
		fmt.Println(err)
	}
}

func (s *memoryArray) saveTasksOnDisk() error {
	v, err := s.tasks.toGOB()
	if err != nil {
		return err
	}

	err = s.db.Set([]byte("key"), v, pebble.Sync)
	if err != nil {
		return err
	}

	return nil
}

func (s *memoryArray) loadTasksFromDisk() error {
	payload, closer, err := s.db.Get([]byte("key"))
	if err != nil {
		if !errors.Is(err, pebble.ErrNotFound) {
			return err
		}

		s.tasks = make([]Task, 0)

		return nil
	}

	closer.Close()

	fmt.Println("initiating from disk")

	s.tasks, err = FromGOB(payload)
	if err != nil {
		return err
	}

	return nil
}
