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
	root            *bPlusPlusTreeNode
	BranchingFactor int // branching factor of B+ Tree
}

// TODO: add lock

type bPlusPlusTreeNode struct {
	isLeaf   bool
	keys     []float64
	children []*bPlusPlusTreeNode
	heap     EightAryHeap // Used only in leaf nodes
	parent   *bPlusPlusTreeNode
	next     *bPlusPlusTreeNode // Pointer to next leaf node (for range queries)
	prev     *bPlusPlusTreeNode // Pointer to prev leaf node (for range queries)
}

func NewBPlusPlusTree(branchingFactor int) BPlusPlusTree {
	return &bPlusPlusTree{
		BranchingFactor: branchingFactor,
		root: &bPlusPlusTreeNode{
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
		newRoot := &bPlusPlusTreeNode{
			keys:     []float64{splitKey},
			children: []*bPlusPlusTreeNode{root, newChild},
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
	nodeMin := tree.findLeafNode(min)
	nodeMax := tree.findLeafNode(max)

	// Scan leaf nodes until max is exceeded
	if nodeMin == nodeMax {
		res := nodeMin.heap.RangeQuery(min, max)
		result = append(result, res...)

		return result
	}

	res := nodeMin.heap.RangeQuery(min, max)
	result = append(result, res...)

	node := nodeMin.next

	for node != nodeMax {
		result = append(result, node.heap.GetData()...)
		node = node.next
	}

	result = append(result, node.heap.RangeQuery(min, max)...)

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

		nextNode := node.next

		// If the node is empty, remove it
		if node.heap.Len() == 0 {
			// remove this leaf node from node.prev.parent
			if node.prev != nil && node.prev.parent != node.parent {
				node.prev.parent.removeNodeFromParent(node)
			}

			// remove leaf node
			node.removeLeafNode()

			// remove this node from memory
			node = nil
		}

		node = nextNode
	}

	return result
}

////////////////////////////////////////////////////////////////////
//       				Helper Functions                          //
////////////////////////////////////////////////////////////////////

// insertRecursive inserts a value into the subtree and returns a split key and new child (if needed)
func (node *bPlusPlusTreeNode) insertRecursive(value float64, branchingFactor int) (float64, *bPlusPlusTreeNode) {
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

	var children []*bPlusPlusTreeNode
	children = append(node.children[:idx+1], append([]*bPlusPlusTreeNode{newChild}, node.children[idx+1:]...)...)
	node.children = children

	// If internal node is full, split it
	if len(node.keys) < branchingFactor {
		return 0, nil
	}

	return node.splitInternal()
}

// insertIntoLeaf inserts a value into a leaf node in sorted order
func (node *bPlusPlusTreeNode) insertIntoLeaf(value float64) {
	// Insert into heap
	node.heap.Insert(value)
}

// splitLeaf splits a full leaf node and returns the split key and new node
func (node *bPlusPlusTreeNode) splitLeaf() (float64, *bPlusPlusTreeNode) {
	newNode := &bPlusPlusTreeNode{
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
func (node *bPlusPlusTreeNode) splitInternal() (float64, *bPlusPlusTreeNode) {
	mid := len(node.keys) / 2
	splitKey := node.keys[mid]

	var newNodeKey []float64
	newNodeKey = append(newNodeKey, node.keys[mid:]...)

	var newNodeChildren []*bPlusPlusTreeNode
	newNodeChildren = append(newNodeChildren, node.children[mid:]...)

	newNode := &bPlusPlusTreeNode{
		keys:     newNodeKey,      // Move half of the keys to new node
		children: newNodeChildren, // Move half of the children to new node
		parent:   nil,             // this parameter set in upper function
		isLeaf:   false,
	}

	var keys []float64
	keys = append(keys, node.keys[:mid]...)
	node.keys = keys // Keep first half in original node

	var children []*bPlusPlusTreeNode
	children = append(children, node.children[:mid+1]...)
	node.children = children // Keep corresponding children

	return splitKey, newNode // Return split key and new internal node
}

// findLeafNode locates the correct leaf node for a given value
func (tree *bPlusPlusTree) findLeafNode(value float64) *bPlusPlusTreeNode {
	node := tree.root
	for !node.isLeaf {
		i := sort.SearchFloat64s(node.keys, value)
		node = node.children[i]
	}

	return node
}

func (node *bPlusPlusTreeNode) removeLeafNode() {
	// step 1: remove this leaf node from node.parent
	node.parent.removeNodeFromParent(node)

	// step 2: update next of prev node
	if node.prev != nil {
		node.prev.next = node.next
	}

	if node.next != nil {
		node.next.prev = node.prev
	}
}

func (node *bPlusPlusTreeNode) removeNodeFromParent(child *bPlusPlusTreeNode) {
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
