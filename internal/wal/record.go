// Package wal implements emberdb's write-ahead log: an append-only file of
// CRC32-checksummed records that makes a transaction durable before any of its
// pages reach the database file.
//
// The log is a redo log of full page images. A transaction appends one record
// per page it touched followed by a commit record; recovery buffers the page
// records and applies them only when it sees the matching commit. A
// transaction whose commit record never made it to disk therefore leaves no
// trace, and one whose commit record did is replayed in full.
package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/anshnt/emberdb/internal/pager"
)

// RecordType identifies what a log record does during replay.
type RecordType uint8

const (
	// RecordBegin marks the start of a transaction. Recovery does not need
	// it, but it makes a log readable and lets tools report transactions
	// that were still in flight when the process died.
	RecordBegin RecordType = 1
	// RecordPage carries the full post-image of one page.
	RecordPage RecordType = 2
	// RecordCommit makes every preceding page record of the same
	// transaction durable, and carries the allocator state the transaction
	// installed.
	RecordCommit RecordType = 3
	// RecordAbort marks a transaction that was rolled back. Its page
	// records, if any, are discarded during replay.
	RecordAbort RecordType = 4
)

// String returns the record type's name.
func (t RecordType) String() string {
	switch t {
	case RecordBegin:
		return "begin"
	case RecordPage:
		return "page"
	case RecordCommit:
		return "commit"
	case RecordAbort:
		return "abort"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}

// Record layout on disk. Every record is framed by a length and a checksum;
// the checksum covers the framed payload, which starts with the type and the
// transaction id.
//
//	 0  4  payload length (uint32)
//	 4  4  CRC32-C of the payload (uint32)
//	 8  1  record type (uint8)
//	 9  8  transaction id (uint64)
//	17  .. type-specific body
const (
	frameSize   = 8
	payloadHead = 9 // type + transaction id
	// stateBodySize is the encoded size of a pager.State inside a commit
	// record: page count, free-list head, free-list count and the meta
	// region.
	stateBodySize = 4 + 4 + 4 + pager.MetaSize
	// pageBodySize is the encoded size of a page record body: the page id
	// followed by the whole page.
	pageBodySize = 4 + pager.PageSize
	// maxPayload bounds what a single record may claim to be, so that a
	// corrupt length field cannot make recovery allocate wildly.
	maxPayload = payloadHead + pageBodySize
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// ErrCorruptRecord reports a record whose checksum does not match its
// contents. Replay treats it as the end of the usable log.
var ErrCorruptRecord = errors.New("emberdb: corrupt log record")

// Record is a decoded log record.
type Record struct {
	// Type is what the record does.
	Type RecordType
	// TxID is the transaction the record belongs to.
	TxID uint64
	// LSN is the record's log sequence number.
	LSN uint64
	// Page is the page a RecordPage carries, and is meaningless otherwise.
	Page pager.PageID
	// Data is the page image a RecordPage carries. It aliases the replay
	// buffer and is only valid until the next record is read.
	Data []byte
	// State is the allocator state a RecordCommit carries.
	State pager.State
}

// encodeState writes a pager.State into a commit record body.
func encodeState(dst []byte, st pager.State) {
	binary.LittleEndian.PutUint32(dst[0:], st.PageCount)
	binary.LittleEndian.PutUint32(dst[4:], uint32(st.FreeHead))
	binary.LittleEndian.PutUint32(dst[8:], st.FreeCount)
	copy(dst[12:12+pager.MetaSize], st.Meta[:])
}

// decodeState reads a pager.State from a commit record body.
func decodeState(src []byte) (pager.State, error) {
	if len(src) < stateBodySize {
		return pager.State{}, fmt.Errorf("%w: commit body is %d bytes, want %d", ErrCorruptRecord, len(src), stateBodySize)
	}
	var st pager.State
	st.PageCount = binary.LittleEndian.Uint32(src[0:])
	st.FreeHead = pager.PageID(binary.LittleEndian.Uint32(src[4:]))
	st.FreeCount = binary.LittleEndian.Uint32(src[8:])
	copy(st.Meta[:], src[12:12+pager.MetaSize])
	if st.PageCount < 1 {
		return pager.State{}, fmt.Errorf("%w: commit record claims %d pages", ErrCorruptRecord, st.PageCount)
	}
	return st, nil
}

// frame writes a complete record into dst, which must be at least
// frameSize+payloadHead+len(body) long, and returns the number of bytes used.
func frame(dst []byte, t RecordType, txID uint64, body []byte) int {
	payloadLen := payloadHead + len(body)
	binary.LittleEndian.PutUint32(dst[0:], uint32(payloadLen))
	payload := dst[frameSize : frameSize+payloadLen]
	payload[0] = byte(t)
	binary.LittleEndian.PutUint64(payload[1:], txID)
	copy(payload[payloadHead:], body)
	binary.LittleEndian.PutUint32(dst[4:], crc32.Checksum(payload, crcTable))
	return frameSize + payloadLen
}
