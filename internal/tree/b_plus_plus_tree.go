package tree

import (
	"sort"
)

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
	heap     EightAryHeap // Used only in leaf nodes
	parent   *bPlusTreeNode
	next     *bPlusTreeNode // Pointer to next leaf node (for range queries)
	prev     *bPlusTreeNode // Pointer to prev leaf node (for range queries)
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
			parent:   nil,
		}

		root.parent = newRoot
		newChild.parent = newRoot

		tree.root = newRoot
	}
}

// RangeQuery returns values within the range [min, max]
func (tree *bPlusPlusTree) RangeQuery(min, max float64) []float64 {
	var result []float64
	node := tree.findLeafNode(min)

	// Scan leaf nodes until max is exceeded
	for node != nil {
		res := node.heap.RangeQuery(min, max)
		result = append(result, res...)

		if len(res) == 0 {
			return result
		}

		node = node.next
	}

	return result
}

// PopRangeQuery removes and returns values within the range [min, max]
func (tree *bPlusPlusTree) PopRangeQuery(min, max float64) []float64 {
	var result []float64
	node := tree.findLeafNode(min)

	// Traverse leaf nodes and remove matching keys
	for node != nil {
		res := node.heap.PopRangeQuery(min, max)
		result = append(result, res...)

		if len(res) == 0 {
			break
		}

		// If the node is empty, remove it
		if node.heap.Len() == 0 {
			// remove this leaf node from node.prev.parent
			if node.prev != nil && node.prev.parent != node.parent {
				node.prev.parent.removeNodeFromParent(node)
			}

			// remove leaf node
			node.removeLeafNode()
		}

		node = node.next
	}

	return result
}

////////////////////////////////////////////////////////////////////
//       				Helper Functions                          //
////////////////////////////////////////////////////////////////////

// insertRecursive inserts a value into the subtree and returns a split key and new child (if needed)
func (node *bPlusTreeNode) insertRecursive(value float64, branchingFactor int) (float64, *bPlusTreeNode) {
	if node.isLeaf {
		node.insertIntoLeaf(value)

		if node.heap.Len() < branchingFactor {
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

	newChild.parent = node

	// Insert new key into internal node
	var keys []float64
	keys = append(node.keys[:idx], append([]float64{splitKey}, node.keys[idx:]...)...)
	node.keys = keys

	var children []*bPlusTreeNode
	children = append(node.children[:idx+1], append([]*bPlusTreeNode{newChild}, node.children[idx+1:]...)...)
	node.children = children

	// If internal node is full, split it
	if len(node.keys) < branchingFactor {
		return 0, nil
	}

	return node.splitInternal()
}

// insertIntoLeaf inserts a value into a leaf node in sorted order
func (node *bPlusTreeNode) insertIntoLeaf(value float64) {
	// Insert into heap
	node.heap.Insert(value)
}

// splitLeaf splits a full leaf node and returns the split key and new node
func (node *bPlusTreeNode) splitLeaf() (float64, *bPlusTreeNode) {
	newNode := &bPlusTreeNode{
		isLeaf: true,
		next:   node.next, // Maintain linked list for range queries
		prev:   node,
		parent: nil,
	}

	if node.next != nil {
		node.next.prev = newNode
	}

	node.next = newNode // Update linked list

	// split heap
	var splitKey float64
	newNode.heap, splitKey = node.heap.Split()

	return splitKey, newNode // Return split key and new leaf
}

// splitInternal splits a full internal node and returns the split key and new node
func (node *bPlusTreeNode) splitInternal() (float64, *bPlusTreeNode) {
	mid := len(node.keys) / 2
	splitKey := node.keys[mid]

	var newNodeKey []float64
	newNodeKey = append(newNodeKey, node.keys[mid:]...)

	var newNodeChildren []*bPlusTreeNode
	newNodeChildren = append(newNodeChildren, node.children[mid:]...)

	newNode := &bPlusTreeNode{
		keys:     newNodeKey,      // Move half of the keys to new node
		children: newNodeChildren, // Move half of the children to new node
		parent:   nil,             // this parameter set in upper function
		isLeaf:   false,
	}

	var keys []float64
	keys = append(keys, node.keys[:mid]...)
	node.keys = keys // Keep first half in original node

	var children []*bPlusTreeNode
	children = append(children, node.children[:mid+1]...)
	node.children = children // Keep corresponding children

	return splitKey, newNode // Return split key and new internal node
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

func (node *bPlusTreeNode) removeLeafNode() {
	// step 1: remove this leaf node from node.parent
	// TODO: check this shit
	node.parent.removeNodeFromParent(node)

	// step 2: update next of prev node
	node.prev.next = node.next
	node.next.prev = node.prev
}

func (node *bPlusTreeNode) removeNodeFromParent(child *bPlusTreeNode) {
	if node.isLeaf {
		return
	}

	// Find the child in the parent's children list
	for i := range node.children {
		if node.children[i] == child {
			if i == len(node.children)-1 {
				node.children = node.children[:i]
				node.keys = node.keys[:i-1]
				break
			}

			// Remove the child reference
			node.children = append(node.children[:i], node.children[i+1:]...)
			node.keys = append(node.keys[:i], node.keys[i+1:]...)
			break
		}
	}

	// If parent is now empty, remove it recursively
	if len(node.children) == 0 {
		node.parent.removeNodeFromParent(node)
	}
}

///////////////////////////////////////////////////////////////////////////////////////
//
//func (tree *bPlusPlusTree) Print() {
//	node := tree.root
//
//	for !node.isLeaf {
//		node = node.children[0]
//	}
//
//	index := 0
//	for node != nil {
//		heapData := node.heap.(*eightAryHeap).values
//		slices.Sort(heapData)
//		fmt.Println("leaf node", index, "keys:", node.keys, "heap:", heapData)
//		node = node.next
//		index++
//	}
//}
