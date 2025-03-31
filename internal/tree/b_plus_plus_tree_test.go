package tree

import (
	"math/rand"
	"testing"
)

func BenchmarkBPlusPlusTreeInsert(b *testing.B) {
	tree := NewBPlusPlusTree(1000)
	for i := 0; i < b.N; i++ {
		tree.Insert(rand.Float64() * 1000000)
	}
}

func BenchmarkBPlusPlusTreeRangeQuery(b *testing.B) {
	tree := NewBPlusPlusTree(1000)
	for i := 0; i < 1000; i++ {
		tree.Insert(rand.Float64() * 1000000)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		min := rand.Float64() * 500000
		max := min + (rand.Float64() * 500000)
		_ = tree.RangeQuery(min, max)
	}
}

func BenchmarkBPlusPlusTreeDeleteRange(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()

		tree := NewBPlusPlusTree(1000)
		for j := 0; j < 1000; j++ {
			tree.Insert(rand.Float64() * 1000000)
		}
		min := rand.Float64() * 500000
		max := min + (rand.Float64() * 500000)

		b.StartTimer()

		_ = tree.PopRangeQuery(min, max)
	}
}
