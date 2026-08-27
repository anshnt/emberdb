package btree

import (
	"fmt"

	"github.com/anshnt/emberdb/internal/pager"
)

// Delete removes key from the tree. It returns the tree's root, which changes
// when the root collapses because its last separator was merged away, and
// whether the key was there to begin with.
func Delete(w WriteStore, root pager.PageID, key []byte) (pager.PageID, bool, error) {
	removed, _, err := deleteFrom(w, root, key)
	if err != nil || !removed {
		return root, false, err
	}
	// Merging can leave the root with a single child and no separators. It
	// is well formed but pointless, so drop the level.
	for {
		n, err := readNode(w, root)
		if err != nil {
			return 0, false, err
		}
		if n.isLeaf() || n.count() > 0 {
			return root, true, nil
		}
		child := n.rightmost()
		if child == 0 {
			return 0, false, fmt.Errorf("%w: root page %d has no children", ErrCorruptNode, root)
		}
		if err := w.Free(root); err != nil {
			return 0, false, err
		}
		root = child
	}
}

// deleteFrom removes key from the subtree rooted at id and reports whether the
// subtree is now too empty for its parent to leave alone.
// Like putInto, the descent is read-only: an internal node only changes when
// the child below it has to be rebalanced.
func deleteFrom(w WriteStore, id pager.PageID, key []byte) (removed, underfull bool, err error) {
	n, err := readNode(w, id)
	if err != nil {
		return false, false, err
	}
	if n.isLeaf() {
		idx, exact, err := n.find(key)
		if err != nil {
			return false, false, err
		}
		if !exact {
			return false, false, nil
		}
		leaf, err := writeNode(w, id)
		if err != nil {
			return false, false, err
		}
		cells := leaf.cells()
		if err := freeCellValue(w, cells[idx]); err != nil {
			return false, false, err
		}
		if err := writeCells(leaf, removeAt(cells, idx)); err != nil {
			return false, false, err
		}
		return true, leaf.usedBytes() < minFill, nil
	}

	ci, err := n.childIndex(key)
	if err != nil {
		return false, false, err
	}
	child, err := n.childAt(ci)
	if err != nil {
		return false, false, err
	}
	removed, childUnderfull, err := deleteFrom(w, child, key)
	if err != nil || !removed {
		return removed, false, err
	}
	if !childUnderfull {
		return true, n.usedBytes() < minFill, nil
	}
	parent, err := writeNode(w, id)
	if err != nil {
		return false, false, err
	}
	if err := rebalance(w, parent, ci); err != nil {
		return false, false, err
	}
	return true, parent.usedBytes() < minFill, nil
}

// rebalance repairs a child that has fallen below minFill, by borrowing one
// entry from a sibling if that leaves the sibling healthy, and by merging the
// two otherwise. Both are best effort: if neither is possible the child simply
// stays sparse, which costs space but never correctness.
func rebalance(w WriteStore, parent node, ci int) error {
	if parent.count() == 0 {
		return nil // an only child has nobody to borrow from
	}
	if ci < parent.count() {
		done, err := borrowFromRight(w, parent, ci)
		if err != nil || done {
			return err
		}
	}
	if ci > 0 {
		done, err := borrowFromLeft(w, parent, ci)
		if err != nil || done {
			return err
		}
	}
	if ci < parent.count() {
		return merge(w, parent, ci)
	}
	return merge(w, parent, ci-1)
}

// setSeparator rewrites the separator at index i so it points at child with
// the given key.
func setSeparator(parent node, i int, child pager.PageID, key []byte) error {
	cells := parent.cells()
	cells[i] = makeInternalCell(child, key)
	return writeCells(parent, cells)
}

// borrowFromRight moves the first entry of child ci+1 into child ci and
// updates the separator between them. It reports false when the sibling cannot
// spare the entry.
func borrowFromRight(w WriteStore, parent node, ci int) (bool, error) {
	leftID, err := parent.childAt(ci)
	if err != nil {
		return false, err
	}
	rightID, err := parent.childAt(ci + 1)
	if err != nil {
		return false, err
	}
	left, err := writeNode(w, leftID)
	if err != nil {
		return false, err
	}
	right, err := writeNode(w, rightID)
	if err != nil {
		return false, err
	}
	if right.count() < 2 {
		return false, nil // taking its last entry would empty it
	}
	rightCells := right.cells()
	if right.usedBytes()-len(rightCells[0])-slotWidth < minFill {
		return false, nil
	}

	var (
		moved        []byte
		newSeparator []byte
		adopted      pager.PageID
	)
	if left.isLeaf() {
		moved = append([]byte(nil), rightCells[0]...)
		next, err := leafKey(rightCells[1])
		if err != nil {
			return false, err
		}
		newSeparator = append([]byte(nil), next...)
	} else {
		separator, err := internalKey(parent.cell(ci))
		if err != nil {
			return false, err
		}
		// The separator drops into the left node to describe the child it
		// is taking over, and the sibling's first separator rises to
		// replace it.
		moved = makeInternalCell(left.rightmost(), separator)
		promoted, err := internalKey(rightCells[0])
		if err != nil {
			return false, err
		}
		newSeparator = append([]byte(nil), promoted...)
		if adopted, err = internalChild(rightCells[0]); err != nil {
			return false, err
		}
	}

	leftCells := append(left.cells(), moved)
	if !cellsFit(leftCells) {
		return false, nil
	}
	if err := writeCells(right, removeAt(rightCells, 0)); err != nil {
		return false, err
	}
	if adopted != 0 {
		left.setRightmost(adopted)
	}
	if err := writeCells(left, leftCells); err != nil {
		return false, err
	}
	return true, setSeparator(parent, ci, leftID, newSeparator)
}

// borrowFromLeft moves the last entry of child ci-1 into child ci and updates
// the separator between them.
func borrowFromLeft(w WriteStore, parent node, ci int) (bool, error) {
	leftID, err := parent.childAt(ci - 1)
	if err != nil {
		return false, err
	}
	rightID, err := parent.childAt(ci)
	if err != nil {
		return false, err
	}
	left, err := writeNode(w, leftID)
	if err != nil {
		return false, err
	}
	right, err := writeNode(w, rightID)
	if err != nil {
		return false, err
	}
	if left.count() < 2 {
		return false, nil
	}
	leftCells := left.cells()
	last := leftCells[len(leftCells)-1]
	if left.usedBytes()-len(last)-slotWidth < minFill {
		return false, nil
	}

	var (
		moved        []byte
		newSeparator []byte
		demoted      pager.PageID
	)
	if right.isLeaf() {
		moved = append([]byte(nil), last...)
		key, err := leafKey(last)
		if err != nil {
			return false, err
		}
		newSeparator = append([]byte(nil), key...)
	} else {
		separator, err := internalKey(parent.cell(ci - 1))
		if err != nil {
			return false, err
		}
		// The separator drops into the right node to describe the child
		// it is taking over, and the sibling's last separator rises to
		// replace it.
		moved = makeInternalCell(left.rightmost(), separator)
		promoted, err := internalKey(last)
		if err != nil {
			return false, err
		}
		newSeparator = append([]byte(nil), promoted...)
		if demoted, err = internalChild(last); err != nil {
			return false, err
		}
	}

	rightCells := insertAt(right.cells(), 0, moved)
	if !cellsFit(rightCells) {
		return false, nil
	}
	if err := writeCells(right, rightCells); err != nil {
		return false, err
	}
	if demoted != 0 {
		left.setRightmost(demoted)
	}
	if err := writeCells(left, leftCells[:len(leftCells)-1]); err != nil {
		return false, err
	}
	return true, setSeparator(parent, ci-1, leftID, newSeparator)
}

// merge folds child i+1 into child i and removes the separator between them.
// For internal children the separator is not discarded but pulled down, since
// it is the only description of the boundary between the two subtrees.
func merge(w WriteStore, parent node, i int) error {
	leftID, err := parent.childAt(i)
	if err != nil {
		return err
	}
	rightID, err := parent.childAt(i + 1)
	if err != nil {
		return err
	}
	left, err := writeNode(w, leftID)
	if err != nil {
		return err
	}
	right, err := writeNode(w, rightID)
	if err != nil {
		return err
	}

	combined := left.cells()
	if !left.isLeaf() {
		separator, err := internalKey(parent.cell(i))
		if err != nil {
			return err
		}
		combined = append(combined, makeInternalCell(left.rightmost(), separator))
	}
	combined = append(combined, right.cells()...)
	if !cellsFit(combined) {
		return nil // leave both sparse rather than lose entries
	}

	if left.isLeaf() {
		following := right.next()
		left.setNext(following)
		if following != 0 {
			after, err := writeNode(w, following)
			if err != nil {
				return err
			}
			after.setPrev(leftID)
		}
	} else {
		left.setRightmost(right.rightmost())
	}
	if err := writeCells(left, combined); err != nil {
		return err
	}
	if err := w.Free(rightID); err != nil {
		return err
	}

	// Drop the separator that described the boundary, and hand the
	// following separator, if any, to the surviving child.
	cells := parent.cells()
	if i+1 == parent.count() {
		parent.setRightmost(leftID)
		return writeCells(parent, cells[:len(cells)-1])
	}
	followingKey, err := internalKey(cells[i+1])
	if err != nil {
		return err
	}
	cells[i] = makeInternalCell(leftID, followingKey)
	return writeCells(parent, removeAt(cells, i+1))
}
