// Package pager implements emberdb's paged file abstraction. It presents a
// single file as an array of fixed-size pages guarded by a double-buffered
// file header, caches recently used pages with an LRU policy, and hands out
// pages from a free list that reuses space reclaimed by earlier deletions.
//
// The pager knows nothing about the contents of a page. Recovery is driven by
// the layer above it: the write-ahead log replays page images through
// ApplyRecovered and restores allocator state through SetRecoveredState.
package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// PageID identifies a page by its ordinal position in the file. Page 0 is the
// header page, so 0 doubles as "no page" in on-disk links.
type PageID uint32

// PageSize is the fixed size in bytes of every page in an emberdb file.
const PageSize = 4096

// MetaSize is the number of bytes in the file header reserved for the layer
// above the pager. The engine stores the catalog root page and the next
// transaction id there.
const MetaSize = 24

// FormatVersion is the on-disk format version stamped into the file header.
// Open refuses a file carrying any other version.
const FormatVersion = 1

// HeaderPage is the page holding the two header slots. The allocator never
// hands it out.
const HeaderPage PageID = 0

// The header page carries two independent copies of the header, "slot A" and
// "slot B". Writes alternate between them so a crash mid-write can never
// destroy the last durable header: the torn slot fails its checksum and the
// intact slot wins. The slots are 2 KiB apart so that they cannot share a
// disk sector.
const (
	slotSize    = 64
	slotAOffset = 0
	slotBOffset = 2048
	slotCount   = 2
)

// Byte offsets within a header slot. docs/format.md carries the same table.
const (
	offMagic     = 0  // 8 bytes
	offVersion   = 8  // uint16
	offPageSize  = 10 // uint16
	offPageCount = 12 // uint32
	offFreeHead  = 16 // uint32
	offFreeCount = 20 // uint32
	offLSN       = 24 // uint64
	offMeta      = 32 // MetaSize bytes
	offChecksum  = 60 // uint32, CRC32-C of bytes [0, 60)
)

var magic = [8]byte{'E', 'M', 'B', 'E', 'R', 'D', 'B', 0x1a}

// crcTable is the Castagnoli polynomial, which has hardware support on both
// amd64 and arm64.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// ErrNotDatabase reports that a file does not begin with the emberdb magic in
// either header slot.
var ErrNotDatabase = errors.New("emberdb: not an emberdb database file")

// ErrCorruptHeader reports that both header slots failed their checksum. The
// file cannot be opened without repair.
var ErrCorruptHeader = errors.New("emberdb: both file header slots are corrupt")

// VersionError reports an on-disk format version the running build cannot
// read.
type VersionError struct {
	Found    uint16
	Expected uint16
}

// Error implements the error interface.
func (e *VersionError) Error() string {
	return fmt.Sprintf("emberdb: file format version %d, this build understands version %d", e.Found, e.Expected)
}

// State is the allocator and engine metadata that a committed transaction
// makes durable. The write-ahead log carries a copy of it in every commit
// record so recovery can restore it without reading the file header.
type State struct {
	// PageCount is the number of pages the file logically contains,
	// including the header page.
	PageCount uint32
	// FreeHead is the first page of the free list, or 0 when it is empty.
	FreeHead PageID
	// FreeCount is the number of pages currently on the free list.
	FreeCount uint32
	// Meta is opaque to the pager and owned by the engine.
	Meta [MetaSize]byte
}

// header is the decoded contents of one header slot.
type header struct {
	state State
	lsn   uint64
}

// encode writes h into a slot-sized buffer, including its checksum.
func (h *header) encode(dst []byte) {
	for i := range dst[:slotSize] {
		dst[i] = 0
	}
	copy(dst[offMagic:], magic[:])
	binary.LittleEndian.PutUint16(dst[offVersion:], FormatVersion)
	binary.LittleEndian.PutUint16(dst[offPageSize:], PageSize)
	binary.LittleEndian.PutUint32(dst[offPageCount:], h.state.PageCount)
	binary.LittleEndian.PutUint32(dst[offFreeHead:], uint32(h.state.FreeHead))
	binary.LittleEndian.PutUint32(dst[offFreeCount:], h.state.FreeCount)
	binary.LittleEndian.PutUint64(dst[offLSN:], h.lsn)
	copy(dst[offMeta:offMeta+MetaSize], h.state.Meta[:])
	binary.LittleEndian.PutUint32(dst[offChecksum:], crc32.Checksum(dst[:offChecksum], crcTable))
}

// decodeHeader parses a slot. It reports whether the slot carries the emberdb
// magic separately from whether it is intact, so that Open can tell a file
// that is not a database from a database whose header is damaged.
func decodeHeader(src []byte) (h header, hasMagic bool, err error) {
	if len(src) < slotSize {
		return header{}, false, ErrCorruptHeader
	}
	hasMagic = string(src[offMagic:offMagic+len(magic)]) == string(magic[:])
	if !hasMagic {
		return header{}, false, ErrNotDatabase
	}
	want := binary.LittleEndian.Uint32(src[offChecksum:])
	if got := crc32.Checksum(src[:offChecksum], crcTable); got != want {
		return header{}, true, ErrCorruptHeader
	}
	if v := binary.LittleEndian.Uint16(src[offVersion:]); v != FormatVersion {
		return header{}, true, &VersionError{Found: v, Expected: FormatVersion}
	}
	if ps := binary.LittleEndian.Uint16(src[offPageSize:]); ps != PageSize {
		return header{}, true, fmt.Errorf("emberdb: file uses %d-byte pages, this build uses %d", ps, PageSize)
	}
	h.state.PageCount = binary.LittleEndian.Uint32(src[offPageCount:])
	h.state.FreeHead = PageID(binary.LittleEndian.Uint32(src[offFreeHead:]))
	h.state.FreeCount = binary.LittleEndian.Uint32(src[offFreeCount:])
	h.lsn = binary.LittleEndian.Uint64(src[offLSN:])
	copy(h.state.Meta[:], src[offMeta:offMeta+MetaSize])
	if h.state.PageCount < 1 {
		return header{}, true, ErrCorruptHeader
	}
	return h, true, nil
}

// slotOffset returns the byte offset of slot i within the header page.
func slotOffset(i int) int {
	if i == 0 {
		return slotAOffset
	}
	return slotBOffset
}
