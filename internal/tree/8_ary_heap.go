package tree

import (
	"container/list"
	"slices"
)

type EightAryHeap interface {
	Insert(val float64)
	RangeQuery(min, max float64) []float64
	PopRangeQuery(min, max float64) []float64
	Len() int
	Split() (EightAryHeap, float64)
}

type eightAryHeap struct {
	values []float64
}

func NewEightAryHeap() EightAryHeap {
	return &eightAryHeap{}
}

// Insert into the heap with 8-ary logic
func (h *eightAryHeap) Insert(val float64) {
	// TODO: don't set duplicate values

	h.values = append(h.values, val)
	h.heapifyUp(len(h.values) - 1)
}

func (h *eightAryHeap) Len() int {
	return len(h.values)
}

// RangeQuery BFS-Based Search
func (h *eightAryHeap) RangeQuery(min, max float64) []float64 {
	var result []float64
	if len(h.values) == 0 {
		return result
	}

	queue := list.New()
	queue.PushBack(0) // Start from root

	for queue.Len() > 0 {
		index := queue.Remove(queue.Front()).(int)

		// If out of range, stop exploring this branch
		if h.values[index] > max {
			continue
		}

		// Add to result if in range
		if h.values[index] >= min {
			result = append(result, h.values[index])
		}

		// Add children (up to 8)
		length := len(h.values) - 1
		for k := 1; k <= 8; k++ {
			childIndex := (index * 8) + k
			if childIndex > length {
				break // No more children
			}

			queue.PushBack(childIndex)
		}
	}

	return result
}

// PopRangeQuery BFS-Based Search with Pop
func (h *eightAryHeap) PopRangeQuery(min, max float64) []float64 {
	var result []float64
	if len(h.values) == 0 {
		return result
	}

	queue := list.New()
	queue.PushBack(0) // Start from root

	for queue.Len() > 0 {
		index := queue.Remove(queue.Front()).(int)

		// If out of range, continue
		if h.values[index] > max {
			continue
		}

		// If within range, remove it immediately
		if h.values[index] >= min {
			result = append(result, h.values[index])

			h.removeAt(index)

			// Since we removed, we need to reprocess the same index
			if index < len(h.values) {
				queue.PushFront(index) // Re-check the swapped element
			}

			continue
		}

		// Add children (up to 8)
		for k := 1; k <= 8; k++ {
			childIndex := (index * 8) + k
			if childIndex >= len(h.values) {
				break
			}

			queue.PushFront(childIndex)
		}
	}

	return result
}

func (h *eightAryHeap) Split() (EightAryHeap, float64) {
	data := h.values
	slices.Sort(data)

	mid := len(data) / 2
	var left, right []float64
	left = append(left, data[:mid]...)
	right = append(right, data[mid:]...)

	h.convertSortedArrayToHeap(left)

	newHeap := new(eightAryHeap)
	newHeap.convertSortedArrayToHeap(right)

	return newHeap, data[mid-1]
}

////////////////////////////////////////////////////////////////////
//       				Helper Functions                          //
////////////////////////////////////////////////////////////////////

func (h *eightAryHeap) swap(i, j int) {
	h.values[i], h.values[j] = h.values[j], h.values[i]
}

func (h *eightAryHeap) heapifyUp(i int) {
	for i > 0 {
		parent := (i - 1) / 8
		if h.values[parent] < h.values[i] {
			break
		}

		h.swap(i, parent)
		i = parent
	}
}

func (h *eightAryHeap) heapifyDown(i int) {
	n := len(h.values)
	for {
		minIndex := i
		for k := 1; k <= 8; k++ {
			childIndex := (i * 8) + k
			if childIndex >= n {
				break
			}

			if h.values[childIndex] < h.values[minIndex] { // Find minimum children value
				minIndex = childIndex
			}
		}

		if minIndex == i {
			break // Heap property restored
		}

		h.swap(i, minIndex)

		i = minIndex
	}
}

func (h *eightAryHeap) removeAt(index int) {
	n := len(h.values)
	if index >= n {
		return
	}

	// Swap with last element and remove
	h.swap(index, n-1)
	h.values = h.values[:n-1]

	// Restore heap property
	if index < len(h.values) {
		h.heapifyDown(index)
	}
}

func (h *eightAryHeap) convertSortedArrayToHeap(arr []float64) {
	h.values = arr
	n := len(h.values)

	// Start heapify from the last non-leaf node
	for i := (n - 1) / 8; i >= 0; i-- {
		h.heapifyDown(i)
	}
}
