package store

import (
	"fmt"

	"github.com/anshnt/emberdb/internal/btree"
	"github.com/anshnt/emberdb/internal/pager"
)

// Snapshot is the view of the database a transaction sees. A row version
// created by transaction x is part of the snapshot when x is the transaction's
// own id, or when x committed before the transaction began.
//
// There is no separate set of in-flight transaction ids to test against.
// emberdb admits one write transaction at a time and hands out ids in commit
// order, so every id at or below Upper has committed and every id above it is
// either in flight or does not exist yet: the bound is the filter.
type Snapshot struct {
	// Self is the transaction's own id, or zero for a read-only
	// transaction, which has none.
	Self uint64
	// Upper is the highest transaction id that had committed when this
	// transaction began.
	Upper uint64
}

// Visible reports whether a row version stamped with txID belongs to this
// snapshot.
func (s Snapshot) Visible(txID uint64) bool {
	if txID == 0 {
		return false
	}
	return txID == s.Self || txID <= s.Upper
}

// Tx is a transaction. Read transactions may run concurrently with each other
// and with a writer; write transactions are serialised.
//
// Every Tx must be finished with Commit or Rollback, which is what releases the
// locks and the snapshot registration it holds.
type Tx struct {
	db       *DB
	writable bool
	id       uint64
	snapshot Snapshot

	// batch collects the transaction's page changes. It is nil for a
	// read-only transaction, which reads straight from the pager.
	batch   *pager.Batch
	catalog pager.PageID

	// tables caches the definitions this transaction has touched, so that a
	// bumped row-id counter or a new index is written back to the catalog
	// once, at commit, rather than on every row.
	tables        map[string]*Table
	schemaChanged bool
	done          bool
}

// Begin starts a transaction. A writable transaction waits for any other
// writer to finish; a read-only one starts immediately.
func (db *DB) Begin(writable bool) (*Tx, error) {
	if db.isClosed() {
		return nil, ErrClosed
	}
	if writable {
		db.writeMu.Lock()
		if db.isClosed() {
			db.writeMu.Unlock()
			return nil, ErrClosed
		}
	}
	db.mu.Lock()
	tx := &Tx{
		db:       db,
		writable: writable,
		snapshot: Snapshot{Upper: db.committed},
		catalog:  db.catalogRoot,
		tables:   make(map[string]*Table),
	}
	if writable {
		tx.id = db.nextTxID
		tx.snapshot.Self = tx.id
	}
	db.snapshots[tx.snapshot.Upper]++
	db.mu.Unlock()

	if writable {
		tx.batch = db.pager.Begin()
	}
	return tx, nil
}

// View runs fn inside a read-only transaction.
func (db *DB) View(fn func(tx *Tx) error) error {
	tx, err := db.Begin(false)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// Update runs fn inside a write transaction, committing when it returns nil
// and rolling back when it returns an error.
func (db *DB) Update(fn func(tx *Tx) error) error {
	tx, err := db.Begin(true)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Writable reports whether the transaction may modify the database.
func (tx *Tx) Writable() bool { return tx.writable }

// ID returns the transaction's id, or zero if it is read-only.
func (tx *Tx) ID() uint64 { return tx.id }

// Snapshot returns the view the transaction reads through.
func (tx *Tx) Snapshot() Snapshot { return tx.snapshot }

// store returns the page source the transaction reads through: its own
// uncommitted pages if it has any, and otherwise the pages as they stood when
// its snapshot was taken.
func (tx *Tx) store() btree.Store {
	if tx.batch != nil {
		return tx.batch
	}
	return snapshotStore{pager: tx.db.pager, upper: tx.snapshot.Upper}
}

// snapshotStore reads pages as of a snapshot bound.
type snapshotStore struct {
	pager *pager.Pager
	upper uint64
}

// Read returns the image of a page this snapshot should see.
func (s snapshotStore) Read(id pager.PageID) ([]byte, error) {
	return s.pager.ReadAt(id, s.upper)
}

// write returns the transaction's page batch, or an error for a read-only
// transaction.
func (tx *Tx) write() (btree.WriteStore, error) {
	if tx.done {
		return nil, ErrTxDone
	}
	if !tx.writable {
		return nil, ErrReadOnlyTx
	}
	return tx.batch, nil
}

// Rollback discards the transaction. Nothing it wrote ever reached the page
// cache, so there is nothing to undo: dropping the batch is the rollback.
func (tx *Tx) Rollback() error {
	if tx.done {
		return nil
	}
	tx.finish()
	return nil
}

// finish releases everything the transaction holds. It is safe to call once.
func (tx *Tx) finish() {
	tx.done = true
	tx.batch = nil
	db := tx.db
	db.mu.Lock()
	if n := db.snapshots[tx.snapshot.Upper]; n <= 1 {
		delete(db.snapshots, tx.snapshot.Upper)
	} else {
		db.snapshots[tx.snapshot.Upper] = n - 1
	}
	oldest := db.oldestSnapshotLocked()
	db.mu.Unlock()
	db.pager.PruneVersions(oldest)
	if tx.writable {
		db.writeMu.Unlock()
	}
}

// Commit makes the transaction's changes durable and visible.
//
// The order is the whole point: every page the transaction touched is written
// to the log and the log is fsynced before a single byte of it is published.
// If the process dies before the fsync returns, replay finds no commit record
// and the transaction never happened; if it dies after, replay redoes it in
// full.
func (tx *Tx) Commit() error {
	if tx.done {
		return ErrTxDone
	}
	if !tx.writable {
		tx.finish()
		return nil
	}
	defer tx.finish()

	if err := tx.flushTables(); err != nil {
		return err
	}
	if tx.batch.Dirty() == 0 {
		// A write transaction that changed nothing is not worth a log
		// record, let alone an fsync.
		return nil
	}

	db := tx.db
	if db.isClosed() {
		return ErrClosed
	}
	tx.batch.SetMeta(encodeMeta(tx.catalog, tx.id+1))

	if err := db.log.Begin(tx.id); err != nil {
		return err
	}
	if err := tx.batch.Images(func(id pager.PageID, data []byte) error {
		return db.log.Page(tx.id, id, data)
	}); err != nil {
		return err
	}
	lsn, err := db.log.Commit(tx.id, tx.batch.State())
	if err != nil {
		return err
	}
	if err := db.log.Sync(lsn); err != nil {
		return fmt.Errorf("emberdb: commit transaction %d: %w", tx.id, err)
	}

	// Past this point the transaction has happened whatever else goes
	// wrong, so publish it.
	//
	// Publishing and advancing the commit counter happen under one lock, so
	// that every transaction either registered before this commit, and gets
	// its superseded pages kept for it, or registers after and sees the new
	// state outright.
	db.mu.Lock()
	retain := len(db.snapshots) > 1
	err = db.pager.Commit(tx.batch, tx.id, retain)
	if err == nil {
		db.catalogRoot = tx.catalog
		db.nextTxID = tx.id + 1
		db.committed = tx.id
	}
	db.mu.Unlock()
	if err != nil {
		return err
	}

	if db.log.Size() >= db.opts.CheckpointBytes {
		return db.checkpoint()
	}
	return nil
}

// oldestSnapshot returns the lowest snapshot bound any open transaction is
// reading through. A row version deleted at or below it is dead for every
// reader alive and every reader still to come, and can be reclaimed.
func (db *DB) oldestSnapshot() uint64 {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.oldestSnapshotLocked()
}

// oldestSnapshotLocked is oldestSnapshot for callers already holding mu.
func (db *DB) oldestSnapshotLocked() uint64 {
	oldest := db.committed
	for upper := range db.snapshots {
		if upper < oldest {
			oldest = upper
		}
	}
	return oldest
}
