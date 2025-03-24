package storage

import (
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/google/uuid"

	"github.com/futureq-io/futureq/internal/config"
)

type memoryArray struct {
	tasks []task
	lock  *sync.RWMutex

	cfg config.Persistence
	db  *pebble.DB
}

func NewMemoryArray(cfg config.Persistence) TaskStorage {
	return &memoryArray{
		tasks: make([]task, 0),
		lock:  new(sync.RWMutex),
		cfg:   cfg,
	}
}

func (s *memoryArray) InitiatePersistence() error {
	id := uuid.New()
	fmt.Println(id.String())

	var err error

	s.db, err = pebble.Open(s.cfg.Path, &pebble.Options{})
	if err != nil {
		return fmt.Errorf("could not open database: %v", err)
	}

	return nil
}

func (s *memoryArray) Add(payload []byte, at time.Time) {
	id := uuid.New().String()
	t := task{id: id, at: at}

	s.lock.Lock()
	defer s.lock.Unlock()

	s.tasks = append(s.tasks, t)

	for i := len(s.tasks) - 1; i > 0; i-- {
		if s.tasks[i].at.Before(s.tasks[i-1].at) {
			s.tasks[i], s.tasks[i-1] = s.tasks[i-1], s.tasks[i]
		}
	}

	_ = s.db.Set([]byte(id), payload, pebble.Sync)
}

func (s *memoryArray) PopLesserThan(v time.Time) []task {
	res, i := s.lesserThan(v)
	s.popFromI(i)

	return res
}

func (s *memoryArray) LesserThan(v time.Time) []task {
	res, _ := s.lesserThan(v)

	return res
}

func (s *memoryArray) lesserThan(v time.Time) ([]task, int) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	result := make([]task, 0)

	var i = 0

	for ; i < len(s.tasks); i++ {
		if s.tasks[i].at.After(v) {
			break
		}

		result = append(result, s.tasks[i])
	}

	for i = 0; i < len(result); i++ {
		payload, closer, _ := s.db.Get([]byte(result[i].id))
		_ = closer.Close()

		_ = s.db.Delete([]byte(result[i].id), nil)

		result[i].payload = payload
	}

	return result, i
}

func (s *memoryArray) popFromI(i int) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.tasks = s.tasks[i:]
}

func (t task) String() string {
	return fmt.Sprintf("{payload:%s, at:%v}", t.payload, t.at)
}
