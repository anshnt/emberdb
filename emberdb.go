// Package emberdb is an embedded, single-file SQL database engine.
//
// A database lives in one file. Opening it gives a handle that runs SQL:
//
//	db, err := emberdb.Open("app.ember")
//	if err != nil {
//		return err
//	}
//	defer db.Close()
//
//	if _, err := db.Exec(`CREATE TABLE notes (title TEXT NOT NULL, body TEXT)`); err != nil {
//		return err
//	}
//	if _, err := db.Exec(`INSERT INTO notes (title, body) VALUES ('first', 'hello')`); err != nil {
//		return err
//	}
//	result, err := db.Query(`SELECT title FROM notes ORDER BY title`)
//
// Statements outside an explicit transaction commit on their own. Use Begin
// for a transaction spanning several statements.
//
// A *DB is safe for concurrent use: any number of goroutines may read at once,
// and one may write. The exception is a transaction opened with the SQL BEGIN
// statement, which belongs to whoever opened it; Begin returns a *Tx for
// programmatic use instead.
package emberdb

import (
	"errors"
	"fmt"
	"sync"

	"github.com/anshnt/emberdb/internal/exec"
	"github.com/anshnt/emberdb/internal/sql"
	"github.com/anshnt/emberdb/internal/store"
	"github.com/anshnt/emberdb/internal/value"
)

// Version is the released version of the library and the ember command.
const Version = "0.1.0"

// Value is a single typed datum: null, an integer, a real, text or a blob.
type Value = value.Value

// Type is a value's storage class.
type Type = value.Type

// The storage classes a column may be declared with.
const (
	TypeNull    = value.TypeNull
	TypeInteger = value.TypeInteger
	TypeReal    = value.TypeReal
	TypeText    = value.TypeText
	TypeBlob    = value.TypeBlob
)

// Null returns the null value.
func Null() Value { return value.Null() }

// Integer returns an integer value.
func Integer(i int64) Value { return value.Integer(i) }

// Real returns a floating-point value.
func Real(f float64) Value { return value.Real(f) }

// Text returns a text value.
func Text(s string) Value { return value.Text(s) }

// Blob returns a blob value, copying b.
func Blob(b []byte) Value { return value.Blob(b) }

// Errors callers are likely to test for.
var (
	// ErrClosed reports use of a database after Close.
	ErrClosed = store.ErrClosed
	// ErrNoSuchTable reports a reference to a table that does not exist.
	ErrNoSuchTable = store.ErrNoSuchTable
	// ErrNoSuchColumn reports a reference to a column that does not exist.
	ErrNoSuchColumn = exec.ErrNoSuchColumn
	// ErrConstraint reports a violated NOT NULL or uniqueness constraint.
	ErrConstraint = store.ErrConstraint
	// ErrTypeMismatch reports a value that does not fit its column's type.
	ErrTypeMismatch = store.ErrTypeMismatch
	// ErrTableExists reports a CREATE TABLE for a name already in use.
	ErrTableExists = store.ErrTableExists
	// ErrIndexExists reports a CREATE INDEX for a name already in use.
	ErrIndexExists = store.ErrIndexExists
	// ErrNoTransaction reports COMMIT or ROLLBACK with none open.
	ErrNoTransaction = exec.ErrNoTransaction
	// ErrTransactionOpen reports BEGIN inside an open transaction.
	ErrTransactionOpen = errors.New("emberdb: a transaction is already open")
)

// SyntaxError is a parse failure, carrying the position of the problem.
type SyntaxError = sql.Error

// Options configures Open.
type Options struct {
	// CacheSize is the page cache capacity in pages. Each page is 4 KiB.
	// Zero selects a 2048-page, 8 MiB cache.
	CacheSize int
	// NoSync disables the fsyncs that make a commit durable. It makes bulk
	// loading and benchmarking faster at the cost of losing recent
	// transactions if the process dies.
	NoSync bool
	// CheckpointBytes is how large the write-ahead log may grow before a
	// commit folds it back into the database file. Zero selects 4 MiB.
	CheckpointBytes int64
}

// Result is what running a statement produced. A SELECT fills Columns and
// Rows; INSERT, UPDATE and DELETE fill RowsAffected.
type Result struct {
	// Columns names the result columns.
	Columns []string
	// Rows holds the result rows, each with one value per column.
	Rows [][]Value
	// RowsAffected counts the rows an INSERT, UPDATE or DELETE touched.
	RowsAffected int
	// LastInsertID is the row id of the last row an INSERT created.
	LastInsertID uint64
	// Plan describes how the statement found its rows, for example
	// "search notes using index notes_by_title".
	Plan string
}

// DB is an open database.
type DB struct {
	store *store.DB

	// mu guards the transaction a SQL BEGIN statement opened. It is held
	// only while inspecting or changing that field, never while a statement
	// runs, so ordinary statements keep the concurrency the store allows.
	mu       sync.Mutex
	explicit *store.Tx
}

// Open opens the database at path, creating it if it does not exist. A
// database left behind by a process that died is recovered here.
func Open(path string) (*DB, error) {
	return OpenWith(path, Options{})
}

// OpenWith opens a database with explicit options.
func OpenWith(path string, opts Options) (*DB, error) {
	underlying, err := store.Open(path, store.Options{
		CacheSize:       opts.CacheSize,
		NoSync:          opts.NoSync,
		CheckpointBytes: opts.CheckpointBytes,
	})
	if err != nil {
		return nil, err
	}
	return &DB{store: underlying}, nil
}

// Close rolls back any open transaction, folds the log into the database file
// and releases it. After a clean close the database is a single file with
// nothing beside it.
func (db *DB) Close() error {
	db.mu.Lock()
	open := db.explicit
	db.explicit = nil
	db.mu.Unlock()
	var rollbackErr error
	if open != nil {
		rollbackErr = open.Rollback()
	}
	return errors.Join(rollbackErr, db.store.Close())
}

// Path returns the database file's path.
func (db *DB) Path() string { return db.store.Path() }

// Exec runs one or more semicolon-separated statements and returns the result
// of the last one.
func (db *DB) Exec(query string) (*Result, error) {
	results, err := db.ExecAll(query)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return &Result{}, nil
	}
	return results[len(results)-1], nil
}

// Query runs a single statement and returns its result. It is Exec under
// another name, for the sake of readable call sites.
func (db *DB) Query(query string) (*Result, error) { return db.Exec(query) }

// ExecAll runs a script and returns one result per statement.
func (db *DB) ExecAll(query string) ([]*Result, error) {
	statements, err := sql.Parse(query)
	if err != nil {
		return nil, err
	}
	results := make([]*Result, 0, len(statements))
	for _, statement := range statements {
		result, err := db.runStatement(statement)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

// runStatement dispatches one statement, handling transaction control here
// because it changes the database handle's state rather than any table.
func (db *DB) runStatement(statement sql.Statement) (*Result, error) {
	switch statement.(type) {
	case *sql.Begin:
		return &Result{}, db.beginExplicit()
	case *sql.Commit:
		return &Result{}, db.endExplicit(true)
	case *sql.Rollback:
		return &Result{}, db.endExplicit(false)
	}

	db.mu.Lock()
	open := db.explicit
	db.mu.Unlock()
	if open != nil {
		result, err := exec.Run(open, statement)
		return convert(result), err
	}
	return db.autoCommit(statement)
}

// autoCommit runs a statement in a transaction of its own.
func (db *DB) autoCommit(statement sql.Statement) (*Result, error) {
	writes := exec.Writes(statement)
	tx, err := db.store.Begin(writes)
	if err != nil {
		return nil, err
	}
	result, err := exec.Run(tx, statement)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return convert(result), nil
}

// beginExplicit opens a transaction that later statements run inside.
func (db *DB) beginExplicit() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.explicit != nil {
		return ErrTransactionOpen
	}
	tx, err := db.store.Begin(true)
	if err != nil {
		return err
	}
	db.explicit = tx
	return nil
}

// endExplicit commits or rolls back the open transaction.
func (db *DB) endExplicit(commit bool) error {
	db.mu.Lock()
	tx := db.explicit
	db.explicit = nil
	db.mu.Unlock()
	if tx == nil {
		return ErrNoTransaction
	}
	if commit {
		return tx.Commit()
	}
	return tx.Rollback()
}

// InTransaction reports whether a SQL BEGIN is currently open.
func (db *DB) InTransaction() bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.explicit != nil
}

// Tx is a transaction spanning several statements.
//
// Every Tx must end in Commit or Rollback; until it does, it holds the
// database's single write slot.
type Tx struct {
	tx   *store.Tx
	done bool
}

// Begin opens a transaction. Statements run through it are not visible to
// anyone else until Commit.
func (db *DB) Begin() (*Tx, error) {
	tx, err := db.store.Begin(true)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx}, nil
}

// View runs fn against a read-only transaction.
func (db *DB) View(fn func(tx *Tx) error) error {
	underlying, err := db.store.Begin(false)
	if err != nil {
		return err
	}
	tx := &Tx{tx: underlying}
	defer tx.Rollback()
	return fn(tx)
}

// Update runs fn against a write transaction, committing when it returns nil
// and rolling back when it returns an error.
func (db *DB) Update(fn func(tx *Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Exec runs one or more statements inside the transaction and returns the
// result of the last one.
func (tx *Tx) Exec(query string) (*Result, error) {
	if tx.done {
		return nil, store.ErrTxDone
	}
	statements, err := sql.Parse(query)
	if err != nil {
		return nil, err
	}
	var last *Result
	for _, statement := range statements {
		switch statement.(type) {
		case *sql.Begin, *sql.Commit, *sql.Rollback:
			return nil, fmt.Errorf("emberdb: use the transaction's own Commit and Rollback, not SQL transaction control")
		}
		result, err := exec.Run(tx.tx, statement)
		if err != nil {
			return nil, err
		}
		last = convert(result)
	}
	if last == nil {
		last = &Result{}
	}
	return last, nil
}

// Commit makes the transaction's changes durable and visible.
func (tx *Tx) Commit() error {
	if tx.done {
		return store.ErrTxDone
	}
	tx.done = true
	return tx.tx.Commit()
}

// Rollback discards the transaction. It is safe to call after Commit, which
// makes it usable in a defer.
func (tx *Tx) Rollback() error {
	if tx.done {
		return nil
	}
	tx.done = true
	return tx.tx.Rollback()
}

// convert turns an internal result into the public one.
func convert(result exec.Result) *Result {
	return &Result{
		Columns:      result.Columns,
		Rows:         result.Rows,
		RowsAffected: result.Changed,
		LastInsertID: result.LastInsertID,
		Plan:         result.Plan,
	}
}
