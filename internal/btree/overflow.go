package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/anshnt/emberdb/internal/pager"
)

// A value too large to sit on a leaf beside its neighbours is written to a
// chain of overflow pages. Each page carries the next page in the chain and
// how many payload bytes it holds:
//
//	0  1  node kind (kindOverflow)
//	1  3  reserved (zero)
//	4  4  next page in the chain, or 0 at the end
//	8  4  payload length in this page
//	12 .. payload
const (
	overflowHeaderSize = 12
	overflowOffNext    = 4
	overflowOffLength  = 8
	overflowCapacity   = pager.PageSize - overflowHeaderSize
)

// writeOverflow stores value in a fresh chain and returns its first page.
func writeOverflow(w WriteStore, value []byte) (pager.PageID, error) {
	if len(value) == 0 {
		return 0, fmt.Errorf("emberdb: refusing to build an overflow chain for an empty value")
	}
	pages := (len(value) + overflowCapacity - 1) / overflowCapacity
	ids := make([]pager.PageID, pages)
	images := make([][]byte, pages)
	for i := 0; i < pages; i++ {
		id, data, err := w.Alloc()
		if err != nil {
			return 0, fmt.Errorf("emberdb: allocate overflow page: %w", err)
		}
		ids[i], images[i] = id, data
	}
	// Fill back to front so each page knows its successor.
	for i := pages - 1; i >= 0; i-- {
		data := images[i]
		data[0] = byte(kindOverflow)
		chunk := value[i*overflowCapacity:]
		if len(chunk) > overflowCapacity {
			chunk = chunk[:overflowCapacity]
		}
		next := pager.PageID(0)
		if i+1 < pages {
			next = ids[i+1]
		}
		binary.LittleEndian.PutUint32(data[overflowOffNext:], uint32(next))
		binary.LittleEndian.PutUint32(data[overflowOffLength:], uint32(len(chunk)))
		copy(data[overflowHeaderSize:], chunk)
	}
	return ids[0], nil
}

// readOverflow reassembles a value of the given total length from its chain.
func readOverflow(s Store, first pager.PageID, total int) ([]byte, error) {
	value := make([]byte, 0, total)
	for id := first; id != 0; {
		data, err := s.Read(id)
		if err != nil {
			return nil, fmt.Errorf("emberdb: read overflow page %d: %w", id, err)
		}
		if kind(data[0]) != kindOverflow {
			return nil, fmt.Errorf("%w: page %d is a %s, not an overflow page", ErrCorruptNode, id, kind(data[0]))
		}
		length := int(binary.LittleEndian.Uint32(data[overflowOffLength:]))
		if length > overflowCapacity || len(value)+length > total {
			return nil, fmt.Errorf("%w: overflow page %d claims %d bytes", ErrCorruptNode, id, length)
		}
		value = append(value, data[overflowHeaderSize:overflowHeaderSize+length]...)
		id = pager.PageID(binary.LittleEndian.Uint32(data[overflowOffNext:]))
	}
	if len(value) != total {
		return nil, fmt.Errorf("%w: overflow chain from page %d yielded %d bytes, want %d", ErrCorruptNode, first, len(value), total)
	}
	return value, nil
}

// freeOverflow returns a whole chain to the allocator.
func freeOverflow(w WriteStore, first pager.PageID) error {
	for id := first; id != 0; {
		data, err := w.Read(id)
		if err != nil {
			return fmt.Errorf("emberdb: read overflow page %d: %w", id, err)
		}
		if kind(data[0]) != kindOverflow {
			return fmt.Errorf("%w: page %d is a %s, not an overflow page", ErrCorruptNode, id, kind(data[0]))
		}
		next := pager.PageID(binary.LittleEndian.Uint32(data[overflowOffNext:]))
		if err := w.Free(id); err != nil {
			return err
		}
		id = next
	}
	return nil
}

// freeCellValue releases the overflow chain a leaf cell owns, if it has one.
func freeCellValue(w WriteStore, cell []byte) error {
	_, _, first, err := leafValue(cell)
	if err != nil {
		return err
	}
	if first == 0 {
		return nil
	}
	return freeOverflow(w, first)
}
