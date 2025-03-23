package storage

import (
	"fmt"
	"sync"
	"time"
)

type memoryArray struct {
	tasks []task
	lock  *sync.RWMutex
}

func NewMemoryArray() TaskStorage {
	return &memoryArray{
		tasks: make([]task, 0),
		lock:  new(sync.RWMutex),
	}
}

func (s *memoryArray) Add(payload []byte, at time.Time) {
	t := task{payload: payload, at: at}

	s.lock.Lock()
	defer s.lock.Unlock()

	s.tasks = append(s.tasks, t)

	for i := len(s.tasks) - 1; i > 0; i-- {
		if s.tasks[i].at.Before(s.tasks[i-1].at) {
			s.tasks[i], s.tasks[i-1] = s.tasks[i-1], s.tasks[i]
		}
	}
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
