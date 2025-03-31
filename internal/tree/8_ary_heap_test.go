package tree

import (
	"math/rand"
	"testing"
)

func Benchmark8AryHeapInsert(b *testing.B) {
	heap := NewEightAryHeap()
	for i := 0; i < b.N; i++ {
		heap.Insert(rand.Float64() * 1000000)
	}
}

func Benchmark8AryHeapRangeQuery(b *testing.B) {
	heap := NewEightAryHeap()
	for i := 0; i < 1000; i++ {
		heap.Insert(rand.Float64() * 1000000)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		min := rand.Float64() * 500000
		max := min + (rand.Float64() * 500000)
		_ = heap.RangeQuery(min, max)
	}
}

func Benchmark8AryHeapDeleteRange(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()

		heap := NewEightAryHeap()
		for j := 0; j < 1000; j++ {
			heap.Insert(rand.Float64() * 1000000)
		}
		min := rand.Float64() * 500000
		max := min + (rand.Float64() * 500000)

		b.StartTimer()

		_ = heap.PopRangeQuery(min, max)
	}
}
