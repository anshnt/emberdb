package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/anshnt/emberdb/internal/btree"
	"github.com/anshnt/emberdb/internal/pager"
	"github.com/anshnt/emberdb/internal/wal"
)

// LogSuffix is appended to the database path to name its write-ahead log. The
// log is transient: a clean shutdown checkpoints and removes it, so a database
// at rest really is a single file.
const LogSuffix = "-wal"

// DefaultCheckpointBytes is how large the log may grow before a commit folds
// it back into the database file.
const DefaultCheckpointBytes = 4 << 20

// The pager reserves a small metadata region in the file header for the layer
// above it. emberdb keeps the catalog's root page and the next transaction id
// there, so both are updated atomically with the pages of the transaction that
// changed them.
const (
	metaOffCatalog  = 0 // uint32
	metaOffNextTxID = 4 // uint64
)

// Errors reported by the store.
var (
	// ErrClosed reports use of a database after Close.
	ErrClosed = errors.New("emberdb: database is closed")
	// ErrTxDone reports use of a transaction after Commit or Rollback.
	ErrTxDone = errors.New("emberdb: transaction is already finished")
	// ErrReadOnlyTx reports an attempt to write in a read-only transaction.
	ErrReadOnlyTx = errors.New("emberdb: transaction is read-only")
	// ErrTableExists reports a CREATE TABLE for a name already in use.
	ErrTableExists = errors.New("emberdb: table already exists")
	// ErrNoSuchTable reports a reference to a table that does not exist.
	ErrNoSuchTable = errors.New("emberdb: no such table")
	// ErrIndexExists reports a CREATE INDEX for a name already in use.
	ErrIndexExists = errors.New("emberdb: index already exists")
	// ErrConstraint reports a violated NOT NULL or uniqueness constraint.
	ErrConstraint = errors.New("emberdb: constraint violation")
	// ErrTypeMismatch reports a value that does not fit its column's type.
	ErrTypeMismatch = errors.New("emberdb: type mismatch")
)

// Options configures Open.
type Options struct {
	// CacheSize is the page cache capacity in pages. Zero picks the pager's
	// default.
	CacheSize int
	// NoSync disables the fsyncs that make commits durable. It is for
	// tests and throughput experiments; a database opened this way does not
	// survive a crash.
	NoSync bool
	// CheckpointBytes is how large the log may grow before a commit folds
	// it into the database file. Zero picks DefaultCheckpointBytes.
	CheckpointBytes int64
}

// DB is an open emberdb database.
//
// Concurrency: any number of read transactions may run at once, and exactly
// one write transaction. Readers never block a writer and a writer never
// blocks a reader. A reader sees the pages as they stood when it began,
// because a commit that supersedes a page keeps the previous image in memory
// while any older transaction is still running.
type DB struct {
	path string
	opts Options

	pager *pager.Pager
	log   *wal.Log

	// writeMu admits one write transaction at a time.
	writeMu sync.Mutex

	// mu guards the fields below, and is held across a commit's publish
	// step so that a transaction cannot register a snapshot in the instant
	// between a writer deciding whether to keep superseded pages and
	// installing the new ones.
	mu          sync.Mutex
	catalogRoot pager.PageID
	nextTxID    uint64
	committed   uint64
	// snapshots counts the open transactions at each snapshot bound, so
	// that a writer can tell which row versions are dead for everyone.
	snapshots map[uint64]int
	closed    bool
}

// Open opens the database at path, creating it if it does not exist, and
// replays the write-ahead log if a previous process died with one in place.
func Open(path string, opts Options) (*DB, error) {
	p, err := pager.Open(path, pager.Options{CacheSize: opts.CacheSize, NoSync: opts.NoSync})
	if err != nil {
		return nil, err
	}
	l, err := wal.Open(path+LogSuffix, wal.Options{NoSync: opts.NoSync})
	if err != nil {
		p.Close()
		return nil, err
	}
	if opts.CheckpointBytes == 0 {
		opts.CheckpointBytes = DefaultCheckpointBytes
	}
	db := &DB{
		path:      path,
		opts:      opts,
		pager:     p,
		log:       l,
		snapshots: make(map[uint64]int),
	}
	if err := db.recover(); err != nil {
		l.Close()
		p.Close()
		return nil, err
	}
	if err := db.bootstrap(); err != nil {
		l.Close()
		p.Close()
		return nil, err
	}
	return db, nil
}

// recover replays the log into the pager and folds the result into the
// database file, so that an open database never depends on a log it has not
// already applied.
func (db *DB) recover() error {
	report, err := db.log.Replay(db.pager.ApplyRecovered)
	if err != nil {
		return err
	}
	lsn := db.pager.LSN()
	if report.Commits > 0 {
		db.pager.SetRecoveredState(report.State)
		lsn = report.LSN
	}
	if err := db.pager.Checkpoint(lsn); err != nil {
		return err
	}
	if err := db.log.Truncate(lsn); err != nil {
		return err
	}
	db.loadMeta()
	return nil
}

// loadMeta reads the catalog root and transaction counter out of the file
// header.
func (db *DB) loadMeta() {
	meta := db.pager.State().Meta
	db.catalogRoot = pager.PageID(binary.LittleEndian.Uint32(meta[metaOffCatalog:]))
	db.nextTxID = binary.LittleEndian.Uint64(meta[metaOffNextTxID:])
	if db.nextTxID < 1 {
		db.nextTxID = 1
	}
	db.committed = db.nextTxID - 1
}

// encodeMeta builds the metadata region a commit will install.
func encodeMeta(catalog pager.PageID, nextTxID uint64) [pager.MetaSize]byte {
	var meta [pager.MetaSize]byte
	binary.LittleEndian.PutUint32(meta[metaOffCatalog:], uint32(catalog))
	binary.LittleEndian.PutUint64(meta[metaOffNextTxID:], nextTxID)
	return meta
}

// bootstrap creates the catalog tree the first time a database is opened.
func (db *DB) bootstrap() error {
	if db.catalogRoot != 0 {
		return nil
	}
	tx, err := db.Begin(true)
	if err != nil {
		return err
	}
	root, err := btree.Create(tx.batch)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("emberdb: create catalog: %w", err)
	}
	tx.catalog = root
	tx.schemaChanged = true
	return tx.Commit()
}

// Path returns the database file's path.
func (db *DB) Path() string { return db.path }

// Close checkpoints the database, removes the log and releases the file. After
// it returns cleanly the database is a single file with nothing beside it.
func (db *DB) Close() error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return nil
	}
	db.closed = true
	db.mu.Unlock()

	checkpointErr := db.checkpoint()
	logErr := db.log.Remove()
	pagerErr := db.pager.Close()
	return errors.Join(checkpointErr, logErr, pagerErr)
}

// checkpoint folds the log into the database file. The caller must hold
// writeMu, so that no commit is midway through appending.
func (db *DB) checkpoint() error {
	lsn := db.log.LSN()
	if err := db.log.Sync(lsn); err != nil {
		return err
	}
	if err := db.pager.Checkpoint(lsn); err != nil {
		return err
	}
	return db.log.Truncate(lsn)
}

// Checkpoint folds the log into the database file on demand.
func (db *DB) Checkpoint() error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()
	if db.isClosed() {
		return ErrClosed
	}
	return db.checkpoint()
}

// isClosed reports whether Close has run.
func (db *DB) isClosed() bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.closed
}

// Stats describes the database's current size and activity.
type Stats struct {
	// Pages is how many pages the file holds, including the header.
	Pages uint32
	// FreePages is how many of them are on the free list.
	FreePages uint32
	// CachedPages is how many pages are in memory.
	CachedPages int
	// PendingPages is how many committed pages await a checkpoint.
	PendingPages int
	// LogBytes is the current size of the write-ahead log.
	LogBytes int64
	// Syncs is how many fsyncs the log has issued.
	Syncs uint64
	// LastTxID is the id of the most recently committed transaction.
	LastTxID uint64
}

// Stats returns a snapshot of the database's counters.
func (db *DB) Stats() Stats {
	state := db.pager.State()
	db.mu.Lock()
	committed := db.committed
	db.mu.Unlock()
	return Stats{
		Pages:        state.PageCount,
		FreePages:    state.FreeCount,
		CachedPages:  db.pager.CachedPages(),
		PendingPages: db.pager.PendingPages(),
		LogBytes:     db.log.Size(),
		Syncs:        db.log.Syncs(),
		LastTxID:     committed,
	}
}

// FileSize returns the size of the database file in bytes.
func (db *DB) FileSize() (int64, error) {
	info, err := os.Stat(db.path)
	if err != nil {
		return 0, fmt.Errorf("emberdb: stat %s: %w", db.path, err)
	}
	return info.Size(), nil
}
