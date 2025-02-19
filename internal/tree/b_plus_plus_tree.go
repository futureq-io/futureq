package tree

import "sort"

type BPlusPlusTree interface {
	Insert(value float64)
	RangeQuery(min, max float64) []float64
	PopRangeQuery(min, max float64) []float64
}

type bPlusPlusTree struct {
	root            *bPlusTreeNode
	BranchingFactor int // branching factor of B+ Tree
}

type bPlusTreeNode struct {
	isLeaf   bool
	keys     []float64
	children []*bPlusTreeNode
	heap     EightAryHeap   // Used only in leaf nodes
	next     *bPlusTreeNode // Pointer to next leaf node (for range queries)
}

func NewBPlusPlusTree(branchingFactor int) BPlusPlusTree {
	return &bPlusPlusTree{
		BranchingFactor: branchingFactor,
		root: &bPlusTreeNode{
			isLeaf: true,
			heap:   NewEightAryHeap(),
		},
	}
}

// Insert inserts a value into the B+ Tree
func (tree *bPlusPlusTree) Insert(value float64) {
	root := tree.root
	splitKey, newChild := root.insertRecursive(value, tree.BranchingFactor)

	// If root was split, create a new root
	if newChild != nil {
		newRoot := &bPlusTreeNode{
			keys:     []float64{splitKey},
			children: []*bPlusTreeNode{root, newChild},
			isLeaf:   false,
		}
		tree.root = newRoot
	}
}

////////////////////////////////////////////////////////////////////
//       				Helper Functions                          //
////////////////////////////////////////////////////////////////////

// insertRecursive inserts a value into the subtree and returns a split key and new child (if needed)
func (node *bPlusTreeNode) insertRecursive(value float64, branchingFactor int) (float64, *bPlusTreeNode) {
	if node.isLeaf {
		node.insertIntoLeaf(value)
		if len(node.keys) < branchingFactor {
			return 0, nil // No need to split
		}

		return node.splitLeaf() // Split leaf if needed
	}

	// Find correct child to insert into
	idx := sort.SearchFloat64s(node.keys, value)
	splitKey, newChild := node.children[idx].insertRecursive(value, branchingFactor)

	// If no split happened, return
	if newChild == nil {
		return 0, nil
	}

	// Insert new key into internal node
	node.keys = append(node.keys[:idx], append([]float64{splitKey}, node.keys[idx:]...)...)
	node.children = append(node.children[:idx+1], append([]*bPlusTreeNode{newChild}, node.children[idx+1:]...)...)

	// If internal node is full, split it
	if len(node.keys) < branchingFactor {
		return 0, nil
	}

	return node.splitInternal()
}

// insertIntoLeaf inserts a value into a leaf node in sorted order
func (node *bPlusTreeNode) insertIntoLeaf(value float64) {
	i := sort.SearchFloat64s(node.keys, value)
	node.keys = append(node.keys[:i], append([]float64{value}, node.keys[i:]...)...)

	// Insert into heap
	node.heap.Insert(value)
}

// splitLeaf splits a full leaf node and returns the split key and new node
func (node *bPlusTreeNode) splitLeaf() (float64, *bPlusTreeNode) {
	mid := len(node.keys) / 2
	newNode := &bPlusTreeNode{
		keys:   node.keys[mid:], // Move half of the keys to new node
		isLeaf: true,
		next:   node.next, // Maintain linked list for range queries
	}
	node.keys = node.keys[:mid] // Keep first half in original node
	node.next = newNode         // Update linked list

	// split heap
	newNode.heap = node.heap.Split()

	return newNode.keys[0], newNode // Return split key and new leaf
}

// splitInternal splits a full internal node and returns the split key and new node
func (node *bPlusTreeNode) splitInternal() (float64, *bPlusTreeNode) {
	mid := len(node.keys) / 2
	splitKey := node.keys[mid]

	newNode := &bPlusTreeNode{
		keys:     node.keys[mid+1:],     // Move half of the keys to new node
		children: node.children[mid+1:], // Move half of the children to new node
		isLeaf:   false,
	}
	node.keys = node.keys[:mid]           // Keep first half in original node
	node.children = node.children[:mid+1] // Keep corresponding children

	return splitKey, newNode // Return split key and new internal node
}

// RangeQuery returns values within the range [min, max]
func (tree *bPlusPlusTree) RangeQuery(min, max float64) []float64 {
	var result []float64
	node := tree.findLeafNode(min)

	// Scan leaf nodes until max is exceeded
	for node != nil {
		res := node.heap.RangeQuery(min, max)
		result = append(result, res...)

		if len(res) != node.heap.Len() {
			return result
		}

		node = node.next
	}

	return result
}

// PopRangeQuery removes and returns values within the range [min, max]
func (tree *BPTree) PopRangeQuery(min, max float64) []float64 {
	var result []float64
	node := tree.findLeafNode(min)

	for node != nil {
		newKeys := node.keys[:0] // Create new slice to store remaining keys
		for _, key := range node.keys {
			if key >= min && key <= max {
				result = append(result, key) // Add to result
			} else {
				newKeys = append(newKeys, key) // Keep the key
			}
		}
		node.keys = newKeys // Update node keys
		node = node.next
	}
	return result
}

// findLeafNode locates the correct leaf node for a given value
func (tree *bPlusPlusTree) findLeafNode(value float64) *bPlusTreeNode {
	node := tree.root
	for !node.isLeaf {
		i := sort.SearchFloat64s(node.keys, value)
		node = node.children[i]
	}

	return node
}
