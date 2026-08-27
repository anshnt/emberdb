// Package btree implements the ordered map emberdb stores tables and indexes
// in: a B+tree over variable-length byte keys and values.
//
// Internal nodes hold separator keys and child pointers; leaves hold keys and
// values and are threaded together by sibling pointers so a range scan reads
// forwards without touching the upper levels again. Values too large to share
// a page with their neighbours spill into an overflow chain.
//
// Every mutation goes through a pager.Batch, so a tree modified inside a
// transaction is invisible to readers until that transaction commits.
package btree

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/anshnt/emberdb/internal/pager"
)

// Node layout. Both node kinds share a 16-byte header, followed by a slot
// directory of uint16 offsets, one per cell, in key order. Cell payloads are
// packed at the end of the page and are always kept contiguous: a mutation
// rewrites the whole cell area rather than leaving holes behind, which trades
// a 4 KiB copy for the absence of a fragmentation bookkeeping problem.
//
//	0  1  node kind
//	1  1  flags (reserved, zero)
//	2  2  cell count (uint16)
//	4  4  reserved (zero)
//	8  4  leaves: next leaf; internal nodes: rightmost child (uint32)
//	12 4  leaves: previous leaf; internal nodes: reserved (uint32)
const (
	nodeHeaderSize = 16
	slotWidth      = 2

	offKind      = 0
	offCount     = 2
	offRight     = 8
	offPrev      = 12
	offSlots     = nodeHeaderSize
	cellCapacity = pager.PageSize - nodeHeaderSize
)

// kind distinguishes the two node layouts.
type kind uint8

const (
	kindInternal kind = 1
	kindLeaf     kind = 2
	kindOverflow kind = 3
)

// String returns the node kind's name.
func (k kind) String() string {
	switch k {
	case kindInternal:
		return "internal"
	case kindLeaf:
		return "leaf"
	case kindOverflow:
		return "overflow"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(k))
	}
}

// MaxKeySize is the largest key the tree accepts. The bound keeps several
// separators on every internal page, which is what stops the tree from
// degenerating into a linked list.
const MaxKeySize = 512

// maxInlineCell is the largest leaf cell stored on the leaf itself. Anything
// larger has its value moved to an overflow chain, so a leaf always holds at
// least four entries no matter how large the values are.
const maxInlineCell = cellCapacity / 4

// leafFlagOverflow marks a leaf cell whose value lives in an overflow chain.
const leafFlagOverflow = 1

// minFill is the number of used bytes below which a node is rebalanced. A
// quarter of the page is the same threshold LMDB and BoltDB settled on: it
// bounds the space a sparse tree can waste without merging on every other
// delete.
const minFill = cellCapacity / 4

// ErrKeyTooLarge reports a key beyond MaxKeySize.
var ErrKeyTooLarge = errors.New("emberdb: key exceeds the maximum key size")

// ErrCorruptNode reports a page that does not decode as a tree node. In a
// well-formed database it means the file has been damaged.
var ErrCorruptNode = errors.New("emberdb: corrupt btree node")

// Store is the read side of the pager, as the tree sees it.
type Store interface {
	// Read returns the current image of a page. The result must not be
	// modified.
	Read(id pager.PageID) ([]byte, error)
}

// WriteStore is the read-write side of the pager: a transaction's batch.
type WriteStore interface {
	Store
	// Writable returns a mutable image of a page, private to the caller's
	// transaction.
	Writable(id pager.PageID) ([]byte, error)
	// Alloc reserves a zeroed page.
	Alloc() (pager.PageID, []byte, error)
	// Free returns a page to the allocator.
	Free(id pager.PageID) error
}

// node is a decoded view over one page. It carries no state of its own; every
// accessor reads or writes the underlying bytes.
type node struct {
	id   pager.PageID
	data []byte
}

// readNode loads a node for reading.
func readNode(s Store, id pager.PageID) (node, error) {
	data, err := s.Read(id)
	if err != nil {
		return node{}, err
	}
	n := node{id: id, data: data}
	return n, n.validate()
}

// writeNode loads a node for modification.
func writeNode(w WriteStore, id pager.PageID) (node, error) {
	data, err := w.Writable(id)
	if err != nil {
		return node{}, err
	}
	n := node{id: id, data: data}
	return n, n.validate()
}

// validate checks the parts of the header a corrupt page could make dangerous.
func (n node) validate() error {
	if len(n.data) != pager.PageSize {
		return fmt.Errorf("%w: page %d is %d bytes", ErrCorruptNode, n.id, len(n.data))
	}
	switch n.kind() {
	case kindInternal, kindLeaf, kindOverflow:
	default:
		return fmt.Errorf("%w: page %d has kind %s", ErrCorruptNode, n.id, n.kind())
	}
	if n.kind() == kindOverflow {
		return nil
	}
	count := n.count()
	if offSlots+count*slotWidth > pager.PageSize {
		return fmt.Errorf("%w: page %d claims %d cells", ErrCorruptNode, n.id, count)
	}
	previous := pager.PageSize
	for i := 0; i < count; i++ {
		off := n.slot(i)
		if off < offSlots+count*slotWidth || off >= previous {
			return fmt.Errorf("%w: page %d slot %d points at offset %d", ErrCorruptNode, n.id, i, off)
		}
		previous = off
	}
	return nil
}

func (n node) kind() kind     { return kind(n.data[offKind]) }
func (n node) isLeaf() bool   { return n.kind() == kindLeaf }
func (n node) count() int     { return int(binary.LittleEndian.Uint16(n.data[offCount:])) }
func (n node) slot(i int) int { return int(binary.LittleEndian.Uint16(n.data[offSlots+i*slotWidth:])) }
func (n node) rightmost() pager.PageID {
	return pager.PageID(binary.LittleEndian.Uint32(n.data[offRight:]))
}
func (n node) next() pager.PageID { return pager.PageID(binary.LittleEndian.Uint32(n.data[offRight:])) }
func (n node) prev() pager.PageID { return pager.PageID(binary.LittleEndian.Uint32(n.data[offPrev:])) }

func (n node) setRightmost(id pager.PageID) {
	binary.LittleEndian.PutUint32(n.data[offRight:], uint32(id))
}
func (n node) setNext(id pager.PageID) {
	binary.LittleEndian.PutUint32(n.data[offRight:], uint32(id))
}
func (n node) setPrev(id pager.PageID) {
	binary.LittleEndian.PutUint32(n.data[offPrev:], uint32(id))
}

// initNode stamps an empty node of the given kind onto a freshly allocated
// page.
func initNode(n node, k kind) {
	for i := range n.data {
		n.data[i] = 0
	}
	n.data[offKind] = byte(k)
}

// cell returns the raw bytes of cell i. The result aliases the page.
//
// writeCells packs cells from the end of the page downwards in slot order, so
// cell i runs from its own slot offset up to the previous cell's. Every
// mutation goes through writeCells, which is what keeps that invariant true
// and makes a cell's length free to compute.
func (n node) cell(i int) []byte {
	start := n.slot(i)
	end := pager.PageSize
	if i > 0 {
		end = n.slot(i - 1)
	}
	return n.data[start:end]
}

// cells returns every cell payload in key order. The results alias the page.
func (n node) cells() [][]byte {
	out := make([][]byte, n.count())
	for i := range out {
		out[i] = n.cell(i)
	}
	return out
}

// usedBytes is how much of the page's cell area is occupied, counting the slot
// directory.
func (n node) usedBytes() int {
	total := n.count() * slotWidth
	for i := 0; i < n.count(); i++ {
		total += len(n.cell(i))
	}
	return total
}

// cellsFit reports whether the given cells can be written into one node.
func cellsFit(cells [][]byte) bool {
	return cellsSize(cells) <= cellCapacity
}

// cellsSize is the page space a set of cells needs, including slots.
func cellsSize(cells [][]byte) int {
	total := len(cells) * slotWidth
	for _, c := range cells {
		total += len(c)
	}
	return total
}

// writeCells replaces a node's contents with cells, keeping its header. It
// builds the new page in a scratch buffer first, so cells may alias the page
// being rewritten.
func writeCells(n node, cells [][]byte) error {
	if !cellsFit(cells) {
		return fmt.Errorf("emberdb: %d cells need %d bytes, page holds %d", len(cells), cellsSize(cells), cellCapacity)
	}
	var buf [pager.PageSize]byte
	copy(buf[:nodeHeaderSize], n.data[:nodeHeaderSize])
	binary.LittleEndian.PutUint16(buf[offCount:], uint16(len(cells)))
	offset := pager.PageSize
	for i, c := range cells {
		offset -= len(c)
		copy(buf[offset:], c)
		binary.LittleEndian.PutUint16(buf[offSlots+i*slotWidth:], uint16(offset))
	}
	copy(n.data, buf[:])
	return nil
}

// splitCells divides cells into two roughly equal halves by byte size. The
// left half always keeps at least one cell, and so does the right.
func splitCells(cells [][]byte) (left, right [][]byte) {
	half := cellsSize(cells) / 2
	running := 0
	split := 0
	for i, c := range cells {
		running += len(c) + slotWidth
		if running >= half {
			split = i + 1
			break
		}
	}
	if split < 1 {
		split = 1
	}
	if split >= len(cells) {
		split = len(cells) - 1
	}
	return cells[:split], cells[split:]
}

// insertAt returns cells with c inserted at index i.
func insertAt(cells [][]byte, i int, c []byte) [][]byte {
	out := make([][]byte, 0, len(cells)+1)
	out = append(out, cells[:i]...)
	out = append(out, c)
	return append(out, cells[i:]...)
}

// removeAt returns cells without index i.
func removeAt(cells [][]byte, i int) [][]byte {
	out := make([][]byte, 0, len(cells)-1)
	out = append(out, cells[:i]...)
	return append(out, cells[i+1:]...)
}
