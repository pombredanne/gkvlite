package gkvlite

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"unsafe"
)

// User-supplied key comparison func should return 0 if a == b,
// -1 if a < b, and +1 if a > b.  For example: bytes.Compare()
type KeyCompare func(a, b []byte) int

// A persistable collection of ordered key-values (Item's).
type Collection struct {
	name    string // May be "" for a private collection.
	store   *Store
	compare KeyCompare

	rootLock *sync.Mutex
	root     *rootNodeLoc // Protected by rootLock.

	stats FreeStats
}

type rootNodeLoc struct {
	// The rootNodeLoc fields are protected by Collection.rootLock.
	refs int64 // Reference counter.
	root *nodeLoc
	next *rootNodeLoc // For free-list tracking.

	// We might own a reference count on another Collection/rootNodeLoc.
	// When our reference drops to 0 and we're free'd, then also release
	// our reference count on the next guy in the chain.
	chainedCollection  *Collection
	chainedRootNodeLoc *rootNodeLoc

	// More nodes to maybe reclaim when our reference count goes to 0.
	// But they might be repeated, so we scan for them during reclaimation.
	reclaimLater [2]*node
}

func (t *Collection) Name() string {
	return t.name
}

func (t *Collection) closeCollection() { // Just "close" is a keyword.
	if t == nil {
		return
	}
	t.rootLock.Lock()
	r := t.root
	t.root = nil
	t.rootLock.Unlock()
	if r != nil {
		t.rootDecRef(r)
	}
}

// Retrieve an item by its key.  Use withValue of false if you don't
// need the item's value (Item.Val may be nil), which might be able
// to save on I/O and memory resources, especially for large values.
// The returned Item should be treated as immutable.
func (t *Collection) GetItem(key []byte, withValue bool) (i *Item, err error) {
	rnl := t.rootAddRef()
	defer t.rootDecRef(rnl)
	n := rnl.root
	for {
		nNode, err := n.read(t.store)
		if err != nil || n.isEmpty() || nNode == nil {
			return nil, err
		}
		i := &nNode.item
		iItem, err := i.read(t, false)
		if err != nil {
			return nil, err
		}
		if iItem == nil || iItem.Key == nil {
			return nil, errors.New("missing item after item.read() in GetItem()")
		}
		c := t.compare(key, iItem.Key)
		if c < 0 {
			n = &nNode.left
		} else if c > 0 {
			n = &nNode.right
		} else {
			if withValue {
				iItem, err = i.read(t, withValue)
				if err != nil {
					return nil, err
				}
			}
			return iItem, nil
		}
	}
}

// Retrieve a value by its key.  Returns nil if the item is not in the
// collection.  The returned value should be treated as immutable.
func (t *Collection) Get(key []byte) (val []byte, err error) {
	i, err := t.GetItem(key, true)
	if err != nil {
		return nil, err
	}
	if i != nil {
		return i.Val, nil
	}
	return nil, nil
}

// Replace or insert an item of a given key.
// A random item Priority (e.g., rand.Int31()) will usually work well,
// but advanced users may consider using non-random item priorities
// at the risk of unbalancing the lookup tree.  The input Item instance
// should be considered immutable and owned by the Collection.
func (t *Collection) SetItem(item *Item) (err error) {
	if item.Key == nil || len(item.Key) > 0xffff || len(item.Key) == 0 ||
		item.Val == nil {
		return errors.New("Item.Key/Val missing or too long")
	}
	if item.Priority < 0 {
		return errors.New("Item.Priority must be non-negative")
	}
	rnl := t.rootAddRef()
	defer t.rootDecRef(rnl)
	root := rnl.root
	n := t.mkNode(nil, nil, nil, 1, uint64(len(item.Key))+uint64(item.NumValBytes(t)))
	n.item.item = unsafe.Pointer(item) // Avoid garbage via separate init.
	nloc := t.mkNodeLoc(n)
	r, err := t.store.union(t, root, nloc)
	if err != nil {
		return err
	}
	rnlNew := t.mkRootNodeLoc(r)
	rnlNew.reclaimLater[0] = n // Can't reclaim n right now because r might point to n.
	if !t.rootCAS(rnl, rnlNew) {
		return errors.New("concurrent mutation attempted")
	}
	t.rootDecRef(rnl)
	t.freeNodeLoc(nloc)
	return nil
}

// Replace or insert an item of a given key.
func (t *Collection) Set(key []byte, val []byte) error {
	return t.SetItem(&Item{Key: key, Val: val, Priority: rand.Int31()})
}

// Deletes an item of a given key.
func (t *Collection) Delete(key []byte) (wasDeleted bool, err error) {
	rnl := t.rootAddRef()
	defer t.rootDecRef(rnl)
	root := rnl.root
	i, err := t.GetItem(key, false)
	if err != nil || i == nil {
		return false, err
	}
	left, middle, right, err := t.store.split(t, root, key)
	if err != nil {
		return false, err
	}
	if middle.isEmpty() {
		return false, fmt.Errorf("concurrent delete, key: %v", key)
	}
	// TODO: Even though we markReclaimable() the middle node, there
	// might not be a pathway to it during reclaimation.
	t.markReclaimable(middle.Node())
	t.freeNodeLoc(middle)
	r, err := t.store.join(t, left, right)
	if err != nil {
		return false, err
	}
	rnlNew := t.mkRootNodeLoc(r)
	if !left.isEmpty() { // Can't reclaim left right now due to readers.
		rnlNew.reclaimLater[0] = left.Node()
	}
	if !right.isEmpty() { // Can't reclaim right right now due to readers.
		rnlNew.reclaimLater[1] = right.Node()
	}
	if !t.rootCAS(rnl, rnlNew) {
		return false, errors.New("concurrent mutation attempted")
	}
	t.rootDecRef(rnl)
	t.freeNodeLoc(left)
	t.freeNodeLoc(right)
	return true, nil
}

// Retrieves the item with the "smallest" key.
// The returned item should be treated as immutable.
func (t *Collection) MinItem(withValue bool) (*Item, error) {
	return t.store.walk(t, withValue,
		func(n *node) (*nodeLoc, bool) { return &n.left, true })
}

// Retrieves the item with the "largest" key.
// The returned item should be treated as immutable.
func (t *Collection) MaxItem(withValue bool) (*Item, error) {
	return t.store.walk(t, withValue,
		func(n *node) (*nodeLoc, bool) { return &n.right, true })
}

// Evict some clean items found by randomly walking a tree branch.
func (t *Collection) EvictSomeItems() (numEvicted uint64) {
	t.store.walk(t, false, func(n *node) (*nodeLoc, bool) {
		if !n.item.Loc().isEmpty() {
			atomic.StorePointer(&n.item.item, unsafe.Pointer(nil))
			numEvicted++
		}
		next := &n.left
		if (rand.Int() & 0x01) == 0x01 {
			next = &n.right
		}
		if next.isEmpty() {
			return nil, false
		}
		return next, true
	})
	return numEvicted
}

type ItemVisitor func(i *Item) bool
type ItemVisitorEx func(i *Item, depth uint64) bool

// Visit items greater-than-or-equal to the target key in ascending order.
func (t *Collection) VisitItemsAscend(target []byte, withValue bool, v ItemVisitor) error {
	return t.VisitItemsAscendEx(target, withValue,
		func(i *Item, depth uint64) bool { return v(i) })
}

// Visit items less-than the target key in descending order.
func (t *Collection) VisitItemsDescend(target []byte, withValue bool, v ItemVisitor) error {
	return t.VisitItemsDescendEx(target, withValue,
		func(i *Item, depth uint64) bool { return v(i) })
}

// Visit items greater-than-or-equal to the target key in ascending order; with depth info.
func (t *Collection) VisitItemsAscendEx(target []byte, withValue bool,
	visitor ItemVisitorEx) error {
	rnl := t.rootAddRef()
	defer t.rootDecRef(rnl)
	_, err := t.store.visitNodes(t, rnl.root,
		target, withValue, visitor, 0, ascendChoice)
	return err
}

// Visit items less-than the target key in descending order; with depth info.
func (t *Collection) VisitItemsDescendEx(target []byte, withValue bool,
	visitor ItemVisitorEx) error {
	rnl := t.rootAddRef()
	defer t.rootDecRef(rnl)
	_, err := t.store.visitNodes(t, rnl.root,
		target, withValue, visitor, 0, descendChoice)
	return err
}

func ascendChoice(cmp int, n *node) (bool, *nodeLoc, *nodeLoc) {
	return cmp <= 0, &n.left, &n.right
}

func descendChoice(cmp int, n *node) (bool, *nodeLoc, *nodeLoc) {
	return cmp > 0, &n.right, &n.left
}

// Returns total number of items and total key bytes plus value bytes.
func (t *Collection) GetTotals() (numItems uint64, numBytes uint64, err error) {
	rnl := t.rootAddRef()
	defer t.rootDecRef(rnl)
	n := rnl.root
	nNode, err := n.read(t.store)
	if err != nil || n.isEmpty() || nNode == nil {
		return 0, 0, err
	}
	return nNode.numNodes, nNode.numBytes, nil
}

// Returns JSON representation of root node file location.
func (t *Collection) MarshalJSON() ([]byte, error) {
	rnl := t.rootAddRef()
	defer t.rootDecRef(rnl)
	loc := rnl.root.Loc()
	if loc.isEmpty() {
		return json.Marshal(ploc_empty)
	}
	return json.Marshal(loc)
}

// Unmarshals JSON representation of root node file location.
func (t *Collection) UnmarshalJSON(d []byte) error {
	p := ploc{}
	if err := json.Unmarshal(d, &p); err != nil {
		return err
	}
	if t.rootLock == nil {
		t.rootLock = &sync.Mutex{}
	}
	nloc := t.mkNodeLoc(nil)
	nloc.loc = unsafe.Pointer(&p)
	if !t.rootCAS(nil, t.mkRootNodeLoc(nloc)) {
		return errors.New("concurrent mutation during UnmarshalJSON().")
	}
	return nil
}

// Writes dirty items of a collection BUT (WARNING) does NOT write new
// root records.  Use Store.Flush() to write root records, which would
// make these writes visible to the next file re-opening/re-loading.
func (t *Collection) Write() (err error) {
	rnl := t.rootAddRef()
	defer t.rootDecRef(rnl)
	root := rnl.root
	if err = t.flushItems(root); err != nil {
		return err
	}
	if err = t.store.flushNodes(root); err != nil {
		return err
	}
	return nil
}

// Assumes that the caller serializes invocations w.r.t. mutations.
func (t *Collection) Stats() FreeStats {
	return t.stats
}

func (t *Collection) flushItems(nloc *nodeLoc) (err error) {
	if nloc == nil || !nloc.Loc().isEmpty() {
		return nil // Flush only unpersisted items of non-empty, unpersisted nodes.
	}
	node := nloc.Node()
	if node == nil {
		return nil
	}
	if err = t.flushItems(&node.left); err != nil {
		return err
	}
	if err = node.item.write(t); err != nil { // Write items in key order.
		return err
	}
	return t.flushItems(&node.right)
}

func (t *Collection) rootCAS(prev, next *rootNodeLoc) bool {
	t.rootLock.Lock()
	defer t.rootLock.Unlock()

	if t.root != prev {
		return false // TODO: Callers need to release resources.
	}
	t.root = next

	if prev != nil && prev.refs > 2 {
		// Since the prev is in-use, hook up its chain to disallow
		// next's nodes from being reclaimed until prev is done.
		if prev.chainedCollection != nil ||
			prev.chainedRootNodeLoc != nil {
			panic(fmt.Sprintf("chain already taken, coll: %v", t.Name()))
		}
		prev.chainedCollection = t
		prev.chainedRootNodeLoc = t.root
		t.root.refs++ // This ref is owned by prev.
	}

	return true
}

func (t *Collection) rootAddRef() *rootNodeLoc {
	t.rootLock.Lock()
	defer t.rootLock.Unlock()
	t.root.refs++
	return t.root
}

func (t *Collection) rootDecRef(r *rootNodeLoc) {
	t.rootLock.Lock()
	freeNodeLock.Lock()
	t.rootDecRef_unlocked(r)
	freeNodeLock.Unlock()
	t.rootLock.Unlock()
}

func (t *Collection) rootDecRef_unlocked(r *rootNodeLoc) {
	r.refs--
	if r.refs > 0 {
		return
	}
	if r.chainedCollection != nil && r.chainedRootNodeLoc != nil {
		r.chainedCollection.rootDecRef_unlocked(r.chainedRootNodeLoc)
	}
	t.reclaimNodes_unlocked(r.root.Node(), &r.reclaimLater)
	for i := 0; i < len(r.reclaimLater); i++ {
		if r.reclaimLater[i] != nil {
			t.reclaimNodes_unlocked(r.reclaimLater[i], nil)
		}
	}
	t.freeNodeLoc(r.root)
	t.freeRootNodeLoc(r)
}
