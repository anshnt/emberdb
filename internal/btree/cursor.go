package btree

import (
	"fmt"

	"github.com/anshnt/emberdb/internal/pager"
)

// Cursor walks a tree's entries in key order. It reads leaves through their
// sibling pointers, so a scan touches the upper levels only once, when it is
// positioned.
//
// A cursor walks in one direction: one from First or Seek is driven with Next,
// one from Last with Prev. The two are not meant to be mixed on the same
// cursor.
//
// The usual shape is a loop:
//
//	c, err := btree.Seek(store, root, from)
//	for c.Next() {
//		if bytes.Compare(c.Key(), to) > 0 {
//			break
//		}
//	}
//	if err := c.Err(); err != nil {
//		return err
//	}
type Cursor struct {
	store   Store
	leaf    node
	idx     int
	pending bool
	valid   bool
	err     error
}

// First returns a cursor positioned before the tree's smallest key.
func First(s Store, root pager.PageID) (*Cursor, error) {
	id := root
	for {
		n, err := readNode(s, id)
		if err != nil {
			return nil, err
		}
		if n.isLeaf() {
			return &Cursor{store: s, leaf: n, idx: 0, pending: true}, nil
		}
		child, err := n.childAt(0)
		if err != nil {
			return nil, err
		}
		if child == 0 {
			return nil, fmt.Errorf("%w: internal page %d has a null first child", ErrCorruptNode, id)
		}
		id = child
	}
}

// Last returns a cursor positioned after the tree's largest key, ready to be
// walked backwards with Prev.
func Last(s Store, root pager.PageID) (*Cursor, error) {
	id := root
	for {
		n, err := readNode(s, id)
		if err != nil {
			return nil, err
		}
		if n.isLeaf() {
			return &Cursor{store: s, leaf: n, idx: n.count(), pending: true}, nil
		}
		child := n.rightmost()
		if child == 0 {
			return nil, fmt.Errorf("%w: internal page %d has a null rightmost child", ErrCorruptNode, id)
		}
		id = child
	}
}

// Seek returns a cursor positioned before the smallest key greater than or
// equal to key.
func Seek(s Store, root pager.PageID, key []byte) (*Cursor, error) {
	id := root
	for {
		n, err := readNode(s, id)
		if err != nil {
			return nil, err
		}
		if n.isLeaf() {
			idx, _, err := n.find(key)
			if err != nil {
				return nil, err
			}
			return &Cursor{store: s, leaf: n, idx: idx, pending: true}, nil
		}
		ci, err := n.childIndex(key)
		if err != nil {
			return nil, err
		}
		child, err := n.childAt(ci)
		if err != nil {
			return nil, err
		}
		if child == 0 {
			return nil, fmt.Errorf("%w: internal page %d has a null child at %d", ErrCorruptNode, id, ci)
		}
		id = child
	}
}

// Next advances to the next entry and reports whether there is one. It must be
// called before the first read.
func (c *Cursor) Next() bool {
	if c.err != nil {
		return false
	}
	if c.pending {
		c.pending = false
	} else if c.valid {
		c.idx++
	} else {
		return false
	}
	// Empty leaves are legal in a tree whose last entries were deleted, so
	// keep walking until an entry turns up or the chain ends.
	for c.idx >= c.leaf.count() {
		following := c.leaf.next()
		if following == 0 {
			c.valid = false
			return false
		}
		next, err := readNode(c.store, following)
		if err != nil {
			c.err = err
			c.valid = false
			return false
		}
		if !next.isLeaf() {
			c.err = fmt.Errorf("%w: page %d is in the leaf chain but is a %s", ErrCorruptNode, following, next.kind())
			c.valid = false
			return false
		}
		c.leaf, c.idx = next, 0
	}
	c.valid = true
	return true
}

// Prev steps back to the previous entry and reports whether there is one. It
// must be called before the first read.
func (c *Cursor) Prev() bool {
	if c.err != nil {
		return false
	}
	if c.pending {
		c.pending = false
		if c.idx >= c.leaf.count() {
			c.idx = c.leaf.count() - 1
		}
	} else if c.valid {
		c.idx--
	} else {
		return false
	}
	for c.idx < 0 {
		preceding := c.leaf.prev()
		if preceding == 0 {
			c.valid = false
			return false
		}
		previous, err := readNode(c.store, preceding)
		if err != nil {
			c.err = err
			c.valid = false
			return false
		}
		if !previous.isLeaf() {
			c.err = fmt.Errorf("%w: page %d is in the leaf chain but is a %s", ErrCorruptNode, preceding, previous.kind())
			c.valid = false
			return false
		}
		c.leaf, c.idx = previous, previous.count()-1
	}
	c.valid = true
	return true
}

// Key returns the current entry's key. It aliases the page cache and is only
// valid until the next call to Next.
func (c *Cursor) Key() []byte {
	if !c.valid {
		return nil
	}
	key, err := leafKey(c.leaf.cell(c.idx))
	if err != nil {
		c.err = err
		return nil
	}
	return key
}

// Value returns a copy of the current entry's value, following its overflow
// chain if it has one.
func (c *Cursor) Value() ([]byte, error) {
	if !c.valid {
		return nil, fmt.Errorf("emberdb: cursor is not positioned on an entry")
	}
	return cellValue(c.store, c.leaf.cell(c.idx))
}

// Err returns the first error the cursor hit, if any. A scan that ended
// because it ran out of entries reports nil.
func (c *Cursor) Err() error { return c.err }
