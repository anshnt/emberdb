package btree

import (
	"encoding/binary"
	"fmt"

	"github.com/anshnt/emberdb/internal/pager"
)

// Leaf cell layout:
//
//	1 byte   flags; leafFlagOverflow means the value is not stored here
//	uvarint  key length, followed by the key
//	uvarint  value length; for an overflow value this is the full length
//	         of the value, not of what follows
//	         inline:   the value bytes
//	         overflow: 4 bytes naming the first page of the chain
//
// Internal cell layout:
//
//	4 bytes  the child page holding keys below this separator
//	uvarint  separator key length, followed by the key

// makeLeafCell builds an inline leaf cell.
func makeLeafCell(key, value []byte) []byte {
	cell := make([]byte, 0, 1+binary.MaxVarintLen64*2+len(key)+len(value))
	cell = append(cell, 0)
	cell = binary.AppendUvarint(cell, uint64(len(key)))
	cell = append(cell, key...)
	cell = binary.AppendUvarint(cell, uint64(len(value)))
	return append(cell, value...)
}

// makeOverflowCell builds a leaf cell whose value lives in a chain starting at
// first.
func makeOverflowCell(key []byte, valueLen int, first pager.PageID) []byte {
	cell := make([]byte, 0, 1+binary.MaxVarintLen64*2+len(key)+4)
	cell = append(cell, leafFlagOverflow)
	cell = binary.AppendUvarint(cell, uint64(len(key)))
	cell = append(cell, key...)
	cell = binary.AppendUvarint(cell, uint64(valueLen))
	var page [4]byte
	binary.LittleEndian.PutUint32(page[:], uint32(first))
	return append(cell, page[:]...)
}

// makeInternalCell builds a separator cell pointing at child.
func makeInternalCell(child pager.PageID, key []byte) []byte {
	cell := make([]byte, 4, 4+binary.MaxVarintLen64+len(key))
	binary.LittleEndian.PutUint32(cell, uint32(child))
	cell = binary.AppendUvarint(cell, uint64(len(key)))
	return append(cell, key...)
}

// leafKey returns the key of a leaf cell. It aliases the cell.
func leafKey(cell []byte) ([]byte, error) {
	if len(cell) < 2 {
		return nil, fmt.Errorf("%w: leaf cell is %d bytes", ErrCorruptNode, len(cell))
	}
	length, n := binary.Uvarint(cell[1:])
	if n <= 0 || 1+n+int(length) > len(cell) {
		return nil, fmt.Errorf("%w: leaf cell has an unreadable key length", ErrCorruptNode)
	}
	return cell[1+n : 1+n+int(length)], nil
}

// leafValue returns a leaf cell's value. An overflow value is reported by its
// total length and the first page of its chain; the returned slice is then nil.
func leafValue(cell []byte) (value []byte, total int, first pager.PageID, err error) {
	if len(cell) < 2 {
		return nil, 0, 0, fmt.Errorf("%w: leaf cell is %d bytes", ErrCorruptNode, len(cell))
	}
	offset := 1
	keyLen, n := binary.Uvarint(cell[offset:])
	if n <= 0 {
		return nil, 0, 0, fmt.Errorf("%w: leaf cell has an unreadable key length", ErrCorruptNode)
	}
	offset += n + int(keyLen)
	if offset >= len(cell) {
		return nil, 0, 0, fmt.Errorf("%w: leaf cell has no value", ErrCorruptNode)
	}
	length, n2 := binary.Uvarint(cell[offset:])
	if n2 <= 0 {
		return nil, 0, 0, fmt.Errorf("%w: leaf cell has an unreadable value length", ErrCorruptNode)
	}
	offset += n2
	if cell[0]&leafFlagOverflow != 0 {
		if offset+4 > len(cell) {
			return nil, 0, 0, fmt.Errorf("%w: overflow cell has no chain pointer", ErrCorruptNode)
		}
		return nil, int(length), pager.PageID(binary.LittleEndian.Uint32(cell[offset:])), nil
	}
	if offset+int(length) > len(cell) {
		return nil, 0, 0, fmt.Errorf("%w: leaf cell claims a %d-byte value", ErrCorruptNode, length)
	}
	return cell[offset : offset+int(length)], int(length), 0, nil
}

// internalChild returns the child page a separator cell points at.
func internalChild(cell []byte) (pager.PageID, error) {
	if len(cell) < 5 {
		return 0, fmt.Errorf("%w: internal cell is %d bytes", ErrCorruptNode, len(cell))
	}
	return pager.PageID(binary.LittleEndian.Uint32(cell)), nil
}

// internalKey returns the separator key of an internal cell. It aliases the
// cell.
func internalKey(cell []byte) ([]byte, error) {
	if len(cell) < 5 {
		return nil, fmt.Errorf("%w: internal cell is %d bytes", ErrCorruptNode, len(cell))
	}
	length, n := binary.Uvarint(cell[4:])
	if n <= 0 || 4+n+int(length) > len(cell) {
		return nil, fmt.Errorf("%w: internal cell has an unreadable key length", ErrCorruptNode)
	}
	return cell[4+n : 4+n+int(length)], nil
}

// cellKey returns the key of a cell belonging to a node of the given kind.
func cellKey(k kind, cell []byte) ([]byte, error) {
	if k == kindLeaf {
		return leafKey(cell)
	}
	return internalKey(cell)
}
