package btree

import (
	"bytes"
	"fmt"

	"github.com/anshnt/emberdb/internal/pager"
)

// Create allocates an empty tree and returns its root page.
func Create(w WriteStore) (pager.PageID, error) {
	id, data, err := w.Alloc()
	if err != nil {
		return 0, fmt.Errorf("emberdb: allocate tree root: %w", err)
	}
	initNode(node{id: id, data: data}, kindLeaf)
	return id, nil
}

// find locates key within a node. It returns the index of the first cell whose
// key is greater than or equal to key, and whether that cell matches exactly.
func (n node) find(key []byte) (int, bool, error) {
	lo, hi := 0, n.count()
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		k, err := cellKey(n.kind(), n.cell(mid))
		if err != nil {
			return 0, false, err
		}
		switch bytes.Compare(k, key) {
		case 0:
			return mid, true, nil
		case -1:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return lo, false, nil
}

// childIndex returns which child of an internal node a key descends into.
func (n node) childIndex(key []byte) (int, error) {
	idx, exact, err := n.find(key)
	if err != nil {
		return 0, err
	}
	if exact {
		// Separators are exclusive on the left: a key equal to separator i
		// lives in the child to its right.
		idx++
	}
	return idx, nil
}

// childAt returns the page of the i-th child, where i ranges over the
// separator count plus one.
func (n node) childAt(i int) (pager.PageID, error) {
	if i < n.count() {
		return internalChild(n.cell(i))
	}
	return n.rightmost(), nil
}

// Get returns the value stored under key. The value is copied out of the page
// cache, so it stays valid after the caller's transaction moves on.
func Get(s Store, root pager.PageID, key []byte) ([]byte, bool, error) {
	id := root
	for {
		n, err := readNode(s, id)
		if err != nil {
			return nil, false, err
		}
		if n.isLeaf() {
			idx, exact, err := n.find(key)
			if err != nil {
				return nil, false, err
			}
			if !exact {
				return nil, false, nil
			}
			value, err := cellValue(s, n.cell(idx))
			if err != nil {
				return nil, false, err
			}
			return value, true, nil
		}
		ci, err := n.childIndex(key)
		if err != nil {
			return nil, false, err
		}
		child, err := n.childAt(ci)
		if err != nil {
			return nil, false, err
		}
		if child == 0 {
			return nil, false, fmt.Errorf("%w: internal page %d has a null child at %d", ErrCorruptNode, id, ci)
		}
		id = child
	}
}

// cellValue materialises a leaf cell's value, following its overflow chain if
// it has one.
func cellValue(s Store, cell []byte) ([]byte, error) {
	inline, total, first, err := leafValue(cell)
	if err != nil {
		return nil, err
	}
	if first != 0 {
		return readOverflow(s, first, total)
	}
	out := make([]byte, len(inline))
	copy(out, inline)
	return out, nil
}

// split describes a node that outgrew its page and had to divide.
type split struct {
	happened bool
	// sep is the smallest key reachable through right, and becomes a
	// separator in the parent.
	sep []byte
	// right is the new page holding the upper half.
	right pager.PageID
}

// Put inserts or replaces the value stored under key and returns the tree's
// root, which changes when the root itself has to split.
func Put(w WriteStore, root pager.PageID, key, value []byte) (pager.PageID, error) {
	if len(key) > MaxKeySize {
		return 0, fmt.Errorf("%w: %d bytes, limit is %d", ErrKeyTooLarge, len(key), MaxKeySize)
	}
	sp, err := putInto(w, root, key, value)
	if err != nil {
		return 0, err
	}
	if !sp.happened {
		return root, nil
	}
	id, data, err := w.Alloc()
	if err != nil {
		return 0, fmt.Errorf("emberdb: allocate new root: %w", err)
	}
	grown := node{id: id, data: data}
	initNode(grown, kindInternal)
	grown.setRightmost(sp.right)
	if err := writeCells(grown, [][]byte{makeInternalCell(root, sp.sep)}); err != nil {
		return 0, err
	}
	return id, nil
}

// putInto inserts into the subtree rooted at id, reporting a split back to the
// caller so the parent can absorb it.
func putInto(w WriteStore, id pager.PageID, key, value []byte) (split, error) {
	n, err := writeNode(w, id)
	if err != nil {
		return split{}, err
	}
	if n.isLeaf() {
		return putIntoLeaf(w, n, key, value)
	}
	ci, err := n.childIndex(key)
	if err != nil {
		return split{}, err
	}
	child, err := n.childAt(ci)
	if err != nil {
		return split{}, err
	}
	sp, err := putInto(w, child, key, value)
	if err != nil || !sp.happened {
		return split{}, err
	}
	// n is still valid: an insert below only ever writes to the subtree it
	// descended into and to the leaf sibling chain, never to an ancestor.
	return absorbSplit(w, n, ci, child, sp)
}

// absorbSplit records a child's split in its parent, splitting the parent in
// turn if the new separator does not fit.
func absorbSplit(w WriteStore, n node, ci int, child pager.PageID, sp split) (split, error) {
	cells := n.cells()
	updated := insertAt(cells, ci, makeInternalCell(child, sp.sep))
	if ci < len(cells) {
		// The separator that used to describe this child now describes the
		// half that split off.
		oldKey, err := internalKey(cells[ci])
		if err != nil {
			return split{}, err
		}
		updated[ci+1] = makeInternalCell(sp.right, oldKey)
	} else {
		n.setRightmost(sp.right)
	}
	if cellsFit(updated) {
		return split{}, writeCells(n, updated)
	}
	return splitInternal(w, n, updated)
}

// putIntoLeaf writes an entry into a leaf, splitting it if the entry does not
// fit.
func putIntoLeaf(w WriteStore, n node, key, value []byte) (split, error) {
	idx, exact, err := n.find(key)
	if err != nil {
		return split{}, err
	}
	cell, err := buildLeafCell(w, key, value)
	if err != nil {
		return split{}, err
	}
	cells := n.cells()
	if exact {
		// Replacing a value orphans whatever the old one overflowed into.
		if err := freeCellValue(w, cells[idx]); err != nil {
			return split{}, err
		}
		cells[idx] = cell
	} else {
		cells = insertAt(cells, idx, cell)
	}
	if cellsFit(cells) {
		return split{}, writeCells(n, cells)
	}
	return splitLeaf(w, n, cells)
}

// buildLeafCell encodes an entry, spilling the value into an overflow chain
// when the cell would crowd out its neighbours.
func buildLeafCell(w WriteStore, key, value []byte) ([]byte, error) {
	cell := makeLeafCell(key, value)
	if len(cell) <= maxInlineCell {
		return cell, nil
	}
	first, err := writeOverflow(w, value)
	if err != nil {
		return nil, err
	}
	return makeOverflowCell(key, len(value), first), nil
}

// splitLeaf divides a leaf's cells across two pages and threads the new page
// into the sibling chain.
func splitLeaf(w WriteStore, n node, cells [][]byte) (split, error) {
	left, right := splitCells(cells)
	id, data, err := w.Alloc()
	if err != nil {
		return split{}, fmt.Errorf("emberdb: allocate leaf: %w", err)
	}
	sibling := node{id: id, data: data}
	initNode(sibling, kindLeaf)

	following := n.next()
	sibling.setNext(following)
	sibling.setPrev(n.id)
	// Write the upper half before rewriting n: those cells still point into
	// n's page image.
	if err := writeCells(sibling, right); err != nil {
		return split{}, err
	}
	sep, err := leafKey(sibling.cell(0))
	if err != nil {
		return split{}, err
	}
	separator := append([]byte(nil), sep...)
	if err := writeCells(n, left); err != nil {
		return split{}, err
	}
	n.setNext(id)
	if following != 0 {
		after, err := writeNode(w, following)
		if err != nil {
			return split{}, err
		}
		after.setPrev(id)
	}
	return split{happened: true, sep: separator, right: id}, nil
}

// splitInternal divides an internal node, promoting the middle separator to
// the parent rather than keeping a copy in either half.
func splitInternal(w WriteStore, n node, cells [][]byte) (split, error) {
	left, right := splitCells(cells)
	// The first cell of the right half is promoted: its key becomes the
	// parent's separator and its child becomes the left half's rightmost.
	// A promoted separator is kept in neither half: its key goes up to the
	// parent and its child becomes the left half's rightmost. If that
	// leaves the right half with no separators it still has its rightmost
	// child, which is a well formed node holding one subtree.
	promoted := right[0]
	right = right[1:]
	sepKey, err := internalKey(promoted)
	if err != nil {
		return split{}, err
	}
	separator := append([]byte(nil), sepKey...)
	leftRightmost, err := internalChild(promoted)
	if err != nil {
		return split{}, err
	}

	id, data, err := w.Alloc()
	if err != nil {
		return split{}, fmt.Errorf("emberdb: allocate internal node: %w", err)
	}
	sibling := node{id: id, data: data}
	initNode(sibling, kindInternal)
	sibling.setRightmost(n.rightmost())
	if err := writeCells(sibling, right); err != nil {
		return split{}, err
	}
	if err := writeCells(n, left); err != nil {
		return split{}, err
	}
	n.setRightmost(leftRightmost)
	return split{happened: true, sep: separator, right: id}, nil
}
