package iradix

import "bytes"

// Tree implements an immutable radix tree. This can be treated as a
// Dictionary abstract data type. The main advantage over a standard
// hash map is prefix-based lookups and ordered iteration. The immutability
// means that it is safe to concurrently read from a Tree without any
// coordination.
type Tree struct {
	root *Node
	size int
}

// New returns an empty Tree
func New() *Tree {
	t := &Tree{
		root: &Node{
			mutateCh: make(chan struct{}),
		},
	}
	return t
}

// Len is used to return the number of elements in the tree
func (t *Tree) Len() int {
	return t.size
}

// Txn is a transaction on the tree. This transaction is applied
// atomically and returns a new tree when committed. A transaction
// is not thread safe, and should only be used by a single goroutine.
type Txn struct {
	root     *Node
	size     int
	modified map[*Node]struct{}

	mutatedNode map[*Node]struct{}
	mutatedLeaf map[*leafNode]struct{}
	trackMutate bool
}

// Txn starts a new transaction that can be used to mutate the tree
func (t *Tree) Txn() *Txn {
	txn := &Txn{
		root: t.root,
		size: t.size,
	}
	return txn
}

// TrackMutate can be used to toggle if mutations are tracked.
// This is used with Notify to cause a notification to the affected
// internal nodes and leaves. Mutations made before enabling tracking
// will not be notified.
func (t *Txn) TrackMutate(track bool) {
	t.trackMutate = track
}

// writeNode returns a ndoe to be modified, if the current
// node as already been modified during the course of
// the transaction, it is used in-place.
func (t *Txn) writeNode(n *Node) *Node {
	// Ensure the modified set exists
	if t.modified == nil {
		t.modified = make(map[*Node]struct{})
	}

	// If this node has already been modified, we can
	// continue to use it during this transaction.
	if _, ok := t.modified[n]; ok {
		return n
	}

	// Mark this node as being mutated
	if t.trackMutate {
		if t.mutatedNode == nil {
			t.mutatedNode = make(map[*Node]struct{})
		}
		t.mutatedNode[n] = struct{}{}
	}

	// Copy the existing node
	nc := new(Node)
	nc.mutateCh = make(chan struct{})
	if n.prefix != nil {
		nc.prefix = make([]byte, len(n.prefix))
		copy(nc.prefix, n.prefix)
	}
	if n.leaf != nil {
		nc.leaf = n.leaf
	}
	if len(n.edges) != 0 {
		nc.edges = make([]edge, len(n.edges))
		copy(nc.edges, n.edges)
	}

	// Mark this node as modified
	t.modified[nc] = struct{}{}
	return nc
}

// mutateLeaf is used to mark a leaf as being mutated
// for the purposes of notification.
func (t *Txn) mutateLeaf(leaf *leafNode) {
	if !t.trackMutate {
		return
	}
	if t.mutatedLeaf == nil {
		t.mutatedLeaf = make(map[*leafNode]struct{})
	}
	t.mutatedLeaf[leaf] = struct{}{}
}

// insert does a recursive insertion
func (t *Txn) insert(n *Node, k, search []byte, v interface{}) (*Node, interface{}, bool) {
	// Handle key exhaution
	if len(search) == 0 {
		nc := t.writeNode(n)
		nc.leaf = &leafNode{
			mutateCh: make(chan struct{}),
			key:      k,
			val:      v,
		}
		if n.isLeaf() {
			t.mutateLeaf(n.leaf)
			return nc, n.leaf.val, true
		} else {
			return nc, nil, false
		}
	}

	// Look for the edge
	idx, child := n.getEdge(search[0])

	// No edge, create one
	if child == nil {
		e := edge{
			label: search[0],
			node: &Node{
				mutateCh: make(chan struct{}),
				leaf: &leafNode{
					mutateCh: make(chan struct{}),
					key:      k,
					val:      v,
				},
				prefix: search,
			},
		}
		nc := t.writeNode(n)
		nc.addEdge(e)
		return nc, nil, false
	}

	// Determine longest prefix of the search key on match
	commonPrefix := longestPrefix(search, child.prefix)
	if commonPrefix == len(child.prefix) {
		search = search[commonPrefix:]
		newChild, oldVal, didUpdate := t.insert(child, k, search, v)
		if newChild != nil {
			nc := t.writeNode(n)
			nc.edges[idx].node = newChild
			return nc, oldVal, didUpdate
		}
		return nil, oldVal, didUpdate
	}

	// Split the node
	nc := t.writeNode(n)
	splitNode := &Node{
		mutateCh: make(chan struct{}),
		prefix:   search[:commonPrefix],
	}
	nc.replaceEdge(edge{
		label: search[0],
		node:  splitNode,
	})

	// Restore the existing child node
	modChild := t.writeNode(child)
	splitNode.addEdge(edge{
		label: modChild.prefix[commonPrefix],
		node:  modChild,
	})
	modChild.prefix = modChild.prefix[commonPrefix:]

	// Create a new leaf node
	leaf := &leafNode{
		mutateCh: make(chan struct{}),
		key:      k,
		val:      v,
	}

	// If the new key is a subset, add to to this node
	search = search[commonPrefix:]
	if len(search) == 0 {
		splitNode.leaf = leaf
		return nc, nil, false
	}

	// Create a new edge for the node
	splitNode.addEdge(edge{
		label: search[0],
		node: &Node{
			mutateCh: make(chan struct{}),
			leaf:     leaf,
			prefix:   search,
		},
	})
	return nc, nil, false
}

// delete does a recursive deletion
func (t *Txn) delete(parent, n *Node, search []byte) (*Node, *leafNode) {
	// Check for key exhaution
	if len(search) == 0 {
		if !n.isLeaf() {
			return nil, nil
		}

		// Remove the leaf node
		nc := t.writeNode(n)
		nc.leaf = nil
		t.mutateLeaf(n.leaf)

		// Check if this node should be merged
		if n != t.root && len(nc.edges) == 1 {
			nc.mergeChild()
		}
		return nc, n.leaf
	}

	// Look for an edge
	label := search[0]
	idx, child := n.getEdge(label)
	if child == nil || !bytes.HasPrefix(search, child.prefix) {
		return nil, nil
	}

	// Consume the search prefix
	search = search[len(child.prefix):]
	newChild, leaf := t.delete(n, child, search)
	if newChild == nil {
		return nil, nil
	}

	// Copy this node
	nc := t.writeNode(n)

	// Delete the edge if the node has no edges
	if newChild.leaf == nil && len(newChild.edges) == 0 {
		nc.delEdge(label)
		if n != t.root && len(nc.edges) == 1 && !nc.isLeaf() {
			nc.mergeChild()
		}
	} else {
		nc.edges[idx].node = newChild
	}
	return nc, leaf
}

// Insert is used to add or update a given key. The return provides
// the previous value and a bool indicating if any was set.
func (t *Txn) Insert(k []byte, v interface{}) (interface{}, bool) {
	newRoot, oldVal, didUpdate := t.insert(t.root, k, k, v)
	if newRoot != nil {
		t.root = newRoot
	}
	if !didUpdate {
		t.size++
	}
	return oldVal, didUpdate
}

// Delete is used to delete a given key. Returns the old value if any,
// and a bool indicating if the key was set.
func (t *Txn) Delete(k []byte) (interface{}, bool) {
	newRoot, leaf := t.delete(nil, t.root, k)
	if newRoot != nil {
		t.root = newRoot
	}
	if leaf != nil {
		t.size--
		return leaf.val, true
	}
	return nil, false
}

// Root returns the current root of the radix tree within this
// transaction. The root is not safe across insert and delete operations,
// but can be used to read the current state during a transaction.
func (t *Txn) Root() *Node {
	return t.root
}

// Get is used to lookup a specific key, returning
// the value and if it was found
func (t *Txn) Get(k []byte) (interface{}, bool) {
	return t.root.Get(k)
}

// GetWatch is used to lookup a specific key, returning
// the watch channel, value and if it was found
func (t *Txn) GetWatch(k []byte) (<-chan struct{}, interface{}, bool) {
	return t.root.GetWatch(k)
}

// Commit is used to finalize the transaction and return a new tree
func (t *Txn) Commit() *Tree {
	nt := &Tree{t.root, t.size}
	t.modified = nil
	return nt
}

// Notify is used along with TrackMutate to trigger notifications.
// It should only be invoked after the transaction has been committed.
func (t *Txn) Notify() {
	for leaf := range t.mutatedLeaf {
		close(leaf.mutateCh)
	}
	for node := range t.mutatedNode {
		close(node.mutateCh)
	}
	t.mutatedNode = nil
	t.mutatedLeaf = nil
}

// Insert is used to add or update a given key. The return provides
// the new tree, previous value and a bool indicating if any was set.
func (t *Tree) Insert(k []byte, v interface{}) (*Tree, interface{}, bool) {
	txn := t.Txn()
	old, ok := txn.Insert(k, v)
	return txn.Commit(), old, ok
}

// Delete is used to delete a given key. Returns the new tree,
// old value if any, and a bool indicating if the key was set.
func (t *Tree) Delete(k []byte) (*Tree, interface{}, bool) {
	txn := t.Txn()
	old, ok := txn.Delete(k)
	return txn.Commit(), old, ok
}

// Root returns the root node of the tree which can be used for richer
// query operations.
func (t *Tree) Root() *Node {
	return t.root
}

// Get is used to lookup a specific key, returning
// the value and if it was found
func (t *Tree) Get(k []byte) (interface{}, bool) {
	return t.root.Get(k)
}

// longestPrefix finds the length of the shared prefix
// of two strings
func longestPrefix(k1, k2 []byte) int {
	max := len(k1)
	if l := len(k2); l < max {
		max = l
	}
	var i int
	for i = 0; i < max; i++ {
		if k1[i] != k2[i] {
			break
		}
	}
	return i
}

// concat two byte slices, returning a third new copy
func concat(a, b []byte) []byte {
	c := make([]byte, len(a)+len(b))
	copy(c, a)
	copy(c[len(a):], b)
	return c
}
