package wal

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/anshnt/emberdb/internal/pager"
)

// Recovery summarises what a Replay found.
type Recovery struct {
	// Commits is the number of transactions replayed.
	Commits int
	// Pages is the number of page images applied.
	Pages int
	// Discarded is the number of transactions whose records were thrown
	// away: rolled back, or still in flight when the process died.
	Discarded int
	// LSN is the sequence number of the last commit record replayed, or the
	// log's base LSN if there was none.
	LSN uint64
	// State is the allocator state the last replayed commit installed. It
	// is only meaningful when Commits is greater than zero.
	State pager.State
	// TornBytes is how many bytes of unusable tail were discarded. A crash
	// mid-append leaves a partial record; it is expected, not an error.
	TornBytes int64
}

// Replay reads the log from the beginning and applies every page image that
// belongs to a committed transaction, in log order.
//
// Page records are buffered until the matching commit record is read, so a
// transaction that was still in flight when the process died contributes
// nothing. Reading stops at the first record that is short or fails its
// checksum: a log is only trustworthy up to its first damaged byte, and
// everything after that point belongs to transactions that never returned from
// commit. The torn tail is truncated so that appends resume from solid ground.
//
// Replay must be called before any append, and only once.
func (l *Log) Replay(apply func(id pager.PageID, data []byte) error) (Recovery, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return Recovery{}, ErrClosed
	}

	rec := Recovery{LSN: l.base}
	pending := make(map[uint64][]pendingPage)
	reader := bufio.NewReaderSize(io.NewSectionReader(l.file, headerSize, l.size-headerSize), writeBufferSize)

	var (
		offset   int64 = headerSize
		records  uint64
		frameBuf = make([]byte, frameSize)
		payload  = make([]byte, maxPayload)
	)
	for {
		if _, err := io.ReadFull(reader, frameBuf); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				break // torn frame header
			}
			return rec, fmt.Errorf("emberdb: read log %s: %w", l.path, err)
		}
		length := binary.LittleEndian.Uint32(frameBuf[0:])
		want := binary.LittleEndian.Uint32(frameBuf[4:])
		if length < payloadHead || length > maxPayload {
			break // a length this implausible means the frame is garbage
		}
		body := payload[:length]
		if _, err := io.ReadFull(reader, body); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break // torn payload
			}
			return rec, fmt.Errorf("emberdb: read log %s: %w", l.path, err)
		}
		if crc32.Checksum(body, crcTable) != want {
			break // the record did not survive; neither does anything after it
		}

		records++
		offset += frameSize + int64(length)
		txID := binary.LittleEndian.Uint64(body[1:])
		switch RecordType(body[0]) {
		case RecordBegin:
			pending[txID] = nil
		case RecordPage:
			page, err := decodePageBody(body[payloadHead:])
			if err != nil {
				return rec, err
			}
			pending[txID] = append(pending[txID], page)
		case RecordAbort:
			if _, ok := pending[txID]; ok {
				rec.Discarded++
			}
			delete(pending, txID)
		case RecordCommit:
			state, err := decodeState(body[payloadHead:])
			if err != nil {
				return rec, err
			}
			for _, page := range pending[txID] {
				if err := apply(page.id, page.data); err != nil {
					return rec, err
				}
				rec.Pages++
			}
			delete(pending, txID)
			rec.Commits++
			rec.LSN = l.base + records
			rec.State = state
		default:
			// An unknown type from a future version cannot be replayed
			// safely, and neither can anything that followed it.
			return rec, fmt.Errorf("%w: unknown record type %d at offset %d", ErrCorruptRecord, body[0], offset)
		}
	}
	rec.Discarded += len(pending)
	rec.TornBytes = l.size - offset

	if err := l.file.Truncate(offset); err != nil {
		return rec, fmt.Errorf("emberdb: truncate log %s: %w", l.path, err)
	}
	l.size = offset
	l.lsn = l.base + records
	l.writer.Reset(newOffsetWriter(l.file, l.size))
	l.synced.Store(l.lsn)
	return rec, nil
}

// pendingPage is a page image waiting for its transaction to commit.
type pendingPage struct {
	id   pager.PageID
	data []byte
}

// decodePageBody copies a page record body out of the shared read buffer.
func decodePageBody(src []byte) (pendingPage, error) {
	if len(src) != pageBodySize {
		return pendingPage{}, fmt.Errorf("%w: page body is %d bytes, want %d", ErrCorruptRecord, len(src), pageBodySize)
	}
	data := make([]byte, pager.PageSize)
	copy(data, src[4:])
	return pendingPage{id: pager.PageID(binary.LittleEndian.Uint32(src)), data: data}, nil
}
