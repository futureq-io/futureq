package heap

import (
	"errors"
	"sync"
)

type EightAryHeap interface {
	Len() int
	GetMin() (int, error)
	Insert(val int)
	Pop() (int, error)
}

// EightAryHeap 8-ary Min Heap Struct
type eightAryHeap struct {
	data []int
	mu   sync.Mutex // For concurrent safety
}

const (
	D           = 8    // 8-ary heap
	InitialSize = 1000 // Preallocate memory for efficiency
)

var ErrEmptyHeap = errors.New("heap is empty")

// NewEightAryHeap initializes an 8-ary heap with preallocated memory
func NewEightAryHeap() EightAryHeap {
	return &eightAryHeap{
		data: make([]int, 0, InitialSize), // Preallocated slice
	}
}

// Len returns heap size
func (h *eightAryHeap) Len() int {
	return len(h.data)
}

// GetMin returns the smallest element (O(1))
func (h *eightAryHeap) GetMin() (int, error) {
	if len(h.data) == 0 {
		return 0, ErrEmptyHeap
	}

	return h.data[0], nil
}

// Insert O(log_8 n), with optimized memory allocation
func (h *eightAryHeap) Insert(val int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.data = append(h.data, val)
	h.up(len(h.data) - 1)
}

// Pop O(8 log_8 n), optimized without using `heap.Pop()`
func (h *eightAryHeap) Pop() (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.data) == 0 {
		return 0, ErrEmptyHeap
	}

	minData := h.data[0]
	last := h.data[len(h.data)-1]
	h.data = h.data[:len(h.data)-1] // Remove last element

	if len(h.data) > 0 {
		h.data[0] = last // Move last to root
		h.down(0)        // Restore heap property
	}

	return minData, nil
}

// Down-heapify for RemoveMin
func (h *eightAryHeap) down(i int) {
	n := len(h.data)
	for {
		minIdx := i
		for j := 1; j <= D; j++ { // Check all D children
			child := D*i + j
			if child < n && h.data[child] < h.data[minIdx] {
				minIdx = child
			}
		}

		if minIdx == i {
			break
		}

		h.data[i], h.data[minIdx] = h.data[minIdx], h.data[i]
		i = minIdx
	}
}

// Up-heapify for Insert
func (h *eightAryHeap) up(i int) {
	for i > 0 {
		parent := (i - 1) / D
		if h.data[i] >= h.data[parent] {
			break
		}

		h.data[i], h.data[parent] = h.data[parent], h.data[i]
		i = parent
	}
}
