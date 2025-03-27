package tree

import (
	"fmt"
	"slices"
	"sort"
)

type BPlusPlusTree interface {
	Insert(value float64)
	RangeQuery(min, max float64) []float64
	PopRangeQuery(min, max float64) []float64

	Print()
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
	var prev *bPlusTreeNode // Tracks the previous node in the linked list

	// Traverse leaf nodes and remove matching keys
	for node != nil {
		res := node.heap.PopRangeQuery(min, max)
		result = append(result, res...)

		// If the node is empty, remove it
		if node.heap.Len() == 0 {
			tree.removeLeafNode(node, prev)
		} else {
			prev = node // Update prev only if node still exists
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

// removeLeafNode removes a leaf node and updates parents recursively
func (tree *bPlusPlusTree) removeLeafNode(leaf *bPlusTreeNode, prev *bPlusTreeNode) {
	// Step 1: Update the linked list
	if prev != nil {
		prev.next = leaf.next // Bypass this node in linked list
	} else if tree.root == leaf {
		tree.root = nil // If the only node is removed, reset tree
		return
	}

	// Step 2: Find and remove the leaf from its parent
	tree.removeFromParent(tree.root, leaf)
}

// removeFromParent removes a child node reference from its parent
func (tree *bPlusPlusTree) removeFromParent(parent, child *bPlusTreeNode) {
	if parent == nil || parent.isLeaf {
		return // No parent found or root is a leaf
	}

	// Find the child in the parent's children list
	for i, c := range parent.children {
		if c == child {
			// Remove the child reference
			parent.children = append(parent.children[:i], parent.children[i+1:]...)
			parent.keys = append(parent.keys[:i], parent.keys[i+1:]...)
			break
		}
	}

	// If parent is now empty, remove it recursively
	if len(parent.children) == 0 {
		tree.removeFromParent(tree.root, parent)
	}
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

/////////////////////////////////////////////////////////////////////////////////////

func (tree *bPlusPlusTree) Print() {
	node := tree.root

	for !node.isLeaf {
		fmt.Println("internal node keys:", node.keys)
		fmt.Println("-------------------------------------")
		node = node.children[0]
	}

	index := 0
	for node != nil {
		heapData := node.heap.(*eightAryHeap).values
		slices.Sort(heapData)
		fmt.Println("leaf node", index, "keys:", node.keys, "heap:", heapData)
		node = node.next
		index++
	}
}
