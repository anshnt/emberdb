package wal

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/anshnt/emberdb/internal/pager"
)

// The log file opens with a fixed header recording the format and the log
// sequence number the first record will follow. Truncating the log after a
// checkpoint rewrites the header with the new base.
//
//	 0  8  magic "EMBERWAL"
//	 8  2  format version (uint16)
//	10  2  page size (uint16)
//	12  8  base LSN (uint64)
//	20  8  reserved (uint64)
//	28  4  CRC32-C of bytes [0, 28)
const (
	headerSize     = 32
	hdrOffVersion  = 8
	hdrOffPageSize = 10
	hdrOffBaseLSN  = 12
	hdrOffChecksum = 28
)

// FormatVersion is the log format this build writes and understands.
const FormatVersion = 1

var logMagic = [8]byte{'E', 'M', 'B', 'E', 'R', 'W', 'A', 'L'}

// writeBufferSize is how much log data is held in user space before a write
// syscall. A commit flushes it before syncing, so buffering never delays
// durability, it only batches the writes of a large transaction.
const writeBufferSize = 1 << 20

// ErrCorruptLogHeader reports a log file whose header is unusable. Unlike a
// corrupt record, which simply ends the usable log, this is fatal: the header
// says where the log begins, so without it nothing after it can be trusted.
var ErrCorruptLogHeader = errors.New("emberdb: corrupt write-ahead log header")

// ErrClosed reports use of a log after Close.
var ErrClosed = errors.New("emberdb: write-ahead log is closed")

// Options configures Open.
type Options struct {
	// NoSync disables the fsync calls that make a commit durable. It makes
	// tests and throughput experiments faster and makes crash recovery
	// meaningless.
	NoSync bool
}

// Log is an append-only write-ahead log. Its exported methods are safe for
// concurrent use, and concurrent commits share fsyncs: see Sync.
type Log struct {
	mu      sync.Mutex
	file    *os.File
	writer  *bufio.Writer
	path    string
	lsn     uint64
	size    int64
	base    uint64
	closed  bool
	noSync  bool
	scratch []byte

	// syncMu serialises fsyncs. A goroutine that finds the log already
	// synced past its target returns without issuing one of its own, so a
	// burst of commits costs a single fsync.
	syncMu sync.Mutex
	synced atomic.Uint64
	syncs  atomic.Uint64
}

// Open opens or creates the log at path. A new log, or one whose header did
// not survive a crash before any record was written, starts empty at base LSN
// zero. Call Replay before appending to a log that already has records.
func Open(path string, opts Options) (*Log, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("emberdb: open log %s: %w", path, err)
	}
	l := &Log{
		file:    file,
		path:    path,
		noSync:  opts.NoSync,
		scratch: make([]byte, frameSize+maxPayload),
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("emberdb: stat log %s: %w", path, err)
	}
	switch {
	case info.Size() < headerSize:
		// Either brand new, or killed before the header was durable. In
		// both cases no record can have been committed, so starting over
		// loses nothing.
		if err := l.writeHeader(0); err != nil {
			file.Close()
			return nil, err
		}
	default:
		base, err := l.readHeader()
		if err != nil {
			file.Close()
			return nil, err
		}
		l.base, l.lsn = base, base
		l.size = info.Size()
	}
	l.writer = bufio.NewWriterSize(newOffsetWriter(l.file, l.size), writeBufferSize)
	l.synced.Store(l.lsn)
	return l, nil
}

// writeHeader stamps a fresh header and truncates the log to it.
func (l *Log) writeHeader(base uint64) error {
	buf := make([]byte, headerSize)
	copy(buf, logMagic[:])
	binary.LittleEndian.PutUint16(buf[hdrOffVersion:], FormatVersion)
	binary.LittleEndian.PutUint16(buf[hdrOffPageSize:], pager.PageSize)
	binary.LittleEndian.PutUint64(buf[hdrOffBaseLSN:], base)
	binary.LittleEndian.PutUint32(buf[hdrOffChecksum:], crc32.Checksum(buf[:hdrOffChecksum], crcTable))
	if err := l.file.Truncate(headerSize); err != nil {
		return fmt.Errorf("emberdb: truncate log %s: %w", l.path, err)
	}
	if _, err := l.file.WriteAt(buf, 0); err != nil {
		return fmt.Errorf("emberdb: write log header: %w", err)
	}
	l.base, l.lsn, l.size = base, base, headerSize
	return l.fsync()
}

// readHeader validates the log header and returns its base LSN.
func (l *Log) readHeader() (uint64, error) {
	buf := make([]byte, headerSize)
	if _, err := l.file.ReadAt(buf, 0); err != nil {
		return 0, fmt.Errorf("emberdb: read log header: %w", err)
	}
	if string(buf[:len(logMagic)]) != string(logMagic[:]) {
		return 0, ErrCorruptLogHeader
	}
	want := binary.LittleEndian.Uint32(buf[hdrOffChecksum:])
	if crc32.Checksum(buf[:hdrOffChecksum], crcTable) != want {
		return 0, ErrCorruptLogHeader
	}
	if v := binary.LittleEndian.Uint16(buf[hdrOffVersion:]); v != FormatVersion {
		return 0, fmt.Errorf("emberdb: log format version %d, this build understands version %d", v, FormatVersion)
	}
	if ps := binary.LittleEndian.Uint16(buf[hdrOffPageSize:]); ps != pager.PageSize {
		return 0, fmt.Errorf("emberdb: log uses %d-byte pages, this build uses %d", ps, pager.PageSize)
	}
	return binary.LittleEndian.Uint64(buf[hdrOffBaseLSN:]), nil
}

// LSN returns the sequence number of the most recently appended record.
func (l *Log) LSN() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lsn
}

// Size returns the log's size in bytes, including anything still buffered. The
// engine uses it to decide when to checkpoint.
func (l *Log) Size() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.size
}

// Syncs returns how many fsyncs the log has issued. Two commits that share one
// fsync increment it once, which is what makes group commit observable.
func (l *Log) Syncs() uint64 { return l.syncs.Load() }

// Path returns the log file's path.
func (l *Log) Path() string { return l.path }

// append frames one record and buffers it, returning its LSN.
func (l *Log) append(t RecordType, txID uint64, body []byte) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(t, txID, body)
}

func (l *Log) appendLocked(t RecordType, txID uint64, body []byte) (uint64, error) {
	if l.closed {
		return 0, ErrClosed
	}
	n := frame(l.scratch, t, txID, body)
	if _, err := l.writer.Write(l.scratch[:n]); err != nil {
		return 0, fmt.Errorf("emberdb: append to log %s: %w", l.path, err)
	}
	l.lsn++
	l.size += int64(n)
	return l.lsn, nil
}

// Begin records the start of a transaction.
func (l *Log) Begin(txID uint64) error {
	_, err := l.append(RecordBegin, txID, nil)
	return err
}

// Abort records that a transaction was rolled back, so a reader of the log can
// tell a deliberate rollback from a crash.
func (l *Log) Abort(txID uint64) error {
	_, err := l.append(RecordAbort, txID, nil)
	return err
}

// Page records the post-image of one page.
func (l *Log) Page(txID uint64, id pager.PageID, data []byte) error {
	if len(data) != pager.PageSize {
		return fmt.Errorf("emberdb: page %d image is %d bytes, want %d", id, len(data), pager.PageSize)
	}
	body := make([]byte, pageBodySize)
	binary.LittleEndian.PutUint32(body, uint32(id))
	copy(body[4:], data)
	_, err := l.append(RecordPage, txID, body)
	return err
}

// Commit records a transaction's commit and returns its LSN. The transaction
// is not durable until Sync reaches that LSN.
func (l *Log) Commit(txID uint64, st pager.State) (uint64, error) {
	body := make([]byte, stateBodySize)
	encodeState(body, st)
	return l.append(RecordCommit, txID, body)
}

// Sync makes every record up to at least target durable.
//
// This is where group commit happens. Only one goroutine syncs at a time; the
// others wait on syncMu, and by the time they get it the leader's fsync has
// usually already covered their target, so they return without issuing one.
// The cost of N concurrent commits is therefore closer to one fsync than to N.
func (l *Log) Sync(target uint64) error {
	if l.synced.Load() >= target {
		return nil
	}
	l.syncMu.Lock()
	defer l.syncMu.Unlock()
	if l.synced.Load() >= target {
		return nil
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return ErrClosed
	}
	if err := l.writer.Flush(); err != nil {
		l.mu.Unlock()
		return fmt.Errorf("emberdb: flush log %s: %w", l.path, err)
	}
	reached := l.lsn
	l.mu.Unlock()

	if err := l.fsync(); err != nil {
		return err
	}
	l.syncs.Add(1)
	// Another leader may have raced ahead; never move the watermark back.
	for {
		current := l.synced.Load()
		if current >= reached || l.synced.CompareAndSwap(current, reached) {
			return nil
		}
	}
}

// fsync flushes the operating system's buffers unless syncing was disabled.
func (l *Log) fsync() error {
	if l.noSync {
		return nil
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("emberdb: sync log %s: %w", l.path, err)
	}
	return nil
}

// Truncate discards the whole log and restarts it at base, which the caller
// must have made durable in the database file header first. Everything the log
// held is by then already in the database file, so dropping it is safe.
func (l *Log) Truncate(base uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	l.writer.Reset(io.Discard)
	if err := l.writeHeader(base); err != nil {
		return err
	}
	l.writer.Reset(newOffsetWriter(l.file, l.size))
	l.synced.Store(l.lsn)
	return nil
}

// Close flushes and closes the log file. It does not sync: a caller that
// wanted durability asked for it at commit time.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if err := l.writer.Flush(); err != nil {
		l.file.Close()
		return fmt.Errorf("emberdb: flush log %s: %w", l.path, err)
	}
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("emberdb: close log %s: %w", l.path, err)
	}
	return nil
}

// Remove closes the log and deletes the file. A cleanly closed database leaves
// no log behind, which is what makes it a single-file database.
func (l *Log) Remove() error {
	if err := l.Close(); err != nil {
		return err
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("emberdb: remove log %s: %w", l.path, err)
	}
	return nil
}

// offsetWriter appends to a file at a tracked offset, so that the log never
// depends on the file's seek position.
type offsetWriter struct {
	file *os.File
	off  int64
}

func newOffsetWriter(file *os.File, off int64) *offsetWriter {
	return &offsetWriter{file: file, off: off}
}

// Write appends p at the writer's offset and advances it.
func (w *offsetWriter) Write(p []byte) (int, error) {
	n, err := w.file.WriteAt(p, w.off)
	w.off += int64(n)
	return n, err
}
