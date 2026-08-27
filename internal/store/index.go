package store

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/anshnt/emberdb/internal/btree"
	"github.com/anshnt/emberdb/internal/value"
)

// An index entry's key is the encoded column value followed by the row id and
// the creating transaction id of the version it describes, both big-endian:
//
//	 0  n  order-preserving encoding of the column value
//	 n  8  row id
//	n+8 8  creating transaction id
//
// Indexing versions rather than rows is what makes an index scan return each
// row exactly once under MVCC: at most one version of a row is visible to a
// given snapshot, so at most one of its entries survives the visibility check.
const indexSuffixSize = 16

// indexKey builds the entry for one version's value.
func indexKey(v value.Value, rowID, xmin uint64) []byte {
	key := value.AppendKey(nil, v)
	var suffix [indexSuffixSize]byte
	binary.BigEndian.PutUint64(suffix[0:], rowID)
	binary.BigEndian.PutUint64(suffix[8:], xmin)
	return append(key, suffix[:]...)
}

// splitIndexKey separates an entry into its encoded value prefix and the
// version it points at.
func splitIndexKey(key []byte) (prefix []byte, rowID, xmin uint64, err error) {
	if len(key) < indexSuffixSize+1 {
		return nil, 0, 0, fmt.Errorf("%w: index key is %d bytes", ErrCorruptRow, len(key))
	}
	cut := len(key) - indexSuffixSize
	return key[:cut], binary.BigEndian.Uint64(key[cut:]), binary.BigEndian.Uint64(key[cut+8:]), nil
}

// addIndexEntries records a row version in every index on its table.
func (tx *Tx) addIndexEntries(t *Table, row []value.Value, rowID, xmin uint64) error {
	w, err := tx.write()
	if err != nil {
		return err
	}
	for i := range t.Indexes {
		index := &t.Indexes[i]
		root, err := btree.Put(w, index.Root, indexKey(row[index.Column], rowID, xmin), nil)
		if err != nil {
			return fmt.Errorf("emberdb: update index %s: %w", index.Name, err)
		}
		index.Root = root
	}
	return nil
}

// removeIndexEntries drops a row version from every index on its table.
func (tx *Tx) removeIndexEntries(t *Table, row []value.Value, rowID, xmin uint64) error {
	w, err := tx.write()
	if err != nil {
		return err
	}
	for i := range t.Indexes {
		index := &t.Indexes[i]
		root, _, err := btree.Delete(w, index.Root, indexKey(row[index.Column], rowID, xmin))
		if err != nil {
			return fmt.Errorf("emberdb: update index %s: %w", index.Name, err)
		}
		index.Root = root
	}
	return nil
}

// checkUnique enforces the unique indexes on a table against a row about to be
// written. Null values never conflict, which is the usual SQL rule.
func (tx *Tx) checkUnique(t *Table, row []value.Value, rowID uint64) error {
	for i := range t.Indexes {
		index := &t.Indexes[i]
		if !index.Unique {
			continue
		}
		v := row[index.Column]
		if v.IsNull() {
			continue
		}
		bound := v
		rows, err := tx.ScanIndex(t, index, Range{Low: &bound, High: &bound})
		if err != nil {
			return err
		}
		for rows.Next() {
			if rows.Row().ID != rowID {
				return fmt.Errorf("%w: %s.%s must be unique, %s is already used",
					ErrConstraint, t.Name, t.Columns[index.Column].Name, v.SQL())
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// Range bounds an index scan. A nil bound is open-ended; an Open bound excludes
// the value itself.
type Range struct {
	// Low is the lower bound, or nil for unbounded.
	Low *value.Value
	// High is the upper bound, or nil for unbounded.
	High *value.Value
	// LowOpen makes the lower bound exclusive.
	LowOpen bool
	// HighOpen makes the upper bound exclusive.
	HighOpen bool
}

// Rows iterates rows a transaction can see. It is not safe for concurrent use.
type Rows struct {
	tx     *Tx
	table  *Table
	index  *Index
	rng    Range
	cursor *btree.Cursor
	row    Row
	err    error
	done   bool
}

// Scan returns every row of a table that this transaction can see, in row-id
// order.
func (tx *Tx) Scan(t *Table) (*Rows, error) {
	if tx.done {
		return nil, ErrTxDone
	}
	c, err := btree.First(tx.store(), t.Root)
	if err != nil {
		return nil, err
	}
	return &Rows{tx: tx, table: t, cursor: c}, nil
}

// ScanIndex returns the rows whose indexed column falls in a range, in index
// order.
func (tx *Tx) ScanIndex(t *Table, index *Index, rng Range) (*Rows, error) {
	if tx.done {
		return nil, ErrTxDone
	}
	var (
		c   *btree.Cursor
		err error
	)
	if rng.Low != nil {
		c, err = btree.Seek(tx.store(), index.Root, value.AppendKey(nil, *rng.Low))
	} else {
		c, err = btree.First(tx.store(), index.Root)
	}
	if err != nil {
		return nil, err
	}
	return &Rows{tx: tx, table: t, index: index, rng: rng, cursor: c}, nil
}

// Next advances to the next visible row and reports whether there is one.
func (r *Rows) Next() bool {
	if r.err != nil || r.done {
		return false
	}
	if r.index != nil {
		return r.nextIndexed()
	}
	return r.nextScanned()
}

// nextScanned walks the table's own tree, skipping versions this transaction
// cannot see.
func (r *Rows) nextScanned() bool {
	for r.cursor.Next() {
		rowID, xmin, err := parseRowKey(r.cursor.Key())
		if err != nil {
			r.err = err
			return false
		}
		encoded, err := r.cursor.Value()
		if err != nil {
			r.err = err
			return false
		}
		xmax, row, err := decodeVersion(encoded, len(r.table.Columns))
		if err != nil {
			r.err = err
			return false
		}
		v := version{rowID: rowID, xmin: xmin, xmax: xmax, row: row}
		if !v.visible(r.tx.snapshot) {
			continue
		}
		r.row = Row{ID: rowID, Values: row}
		return true
	}
	r.err = r.cursor.Err()
	return false
}

// nextIndexed walks the index, reading back each version it points at and
// checking it against the snapshot.
//
// The re-read is not just a visibility check. An index entry can be stale: a
// transaction that updates the same row twice leaves behind an entry for the
// value it wrote first, under the same row id and transaction id. Comparing
// the version's current value against the entry's prefix discards those,
// which is what keeps the scan from returning a row twice.
func (r *Rows) nextIndexed() bool {
	for r.cursor.Next() {
		prefix, rowID, xmin, err := splitIndexKey(r.cursor.Key())
		if err != nil {
			r.err = err
			return false
		}
		decoded, _, err := value.DecodeKey(prefix)
		if err != nil {
			r.err = err
			return false
		}
		if !r.withinRange(decoded) {
			if r.pastRange(decoded) {
				r.done = true
				return false
			}
			continue
		}
		encoded, found, err := btree.Get(r.tx.store(), r.table.Root, rowKey(rowID, xmin))
		if err != nil {
			r.err = err
			return false
		}
		if !found {
			continue // the version was pruned out from under the entry
		}
		xmax, row, err := decodeVersion(encoded, len(r.table.Columns))
		if err != nil {
			r.err = err
			return false
		}
		v := version{rowID: rowID, xmin: xmin, xmax: xmax, row: row}
		if !v.visible(r.tx.snapshot) {
			continue
		}
		if !bytes.Equal(value.AppendKey(nil, row[r.index.Column]), prefix) {
			continue // stale entry from an earlier write in this transaction
		}
		r.row = Row{ID: rowID, Values: row}
		return true
	}
	r.err = r.cursor.Err()
	return false
}

// withinRange reports whether a value satisfies the scan's bounds.
func (r *Rows) withinRange(v value.Value) bool {
	if r.rng.Low != nil {
		c := value.Compare(v, *r.rng.Low)
		if c < 0 || (c == 0 && r.rng.LowOpen) {
			return false
		}
	}
	if r.rng.High != nil {
		c := value.Compare(v, *r.rng.High)
		if c > 0 || (c == 0 && r.rng.HighOpen) {
			return false
		}
	}
	return true
}

// pastRange reports whether the scan has walked beyond its upper bound, which
// means no later entry can match either.
func (r *Rows) pastRange(v value.Value) bool {
	if r.rng.High == nil {
		return false
	}
	c := value.Compare(v, *r.rng.High)
	return c > 0 || (c == 0 && r.rng.HighOpen)
}

// Row returns the row the iterator is positioned on.
func (r *Rows) Row() Row { return r.row }

// Err returns the first error the iteration hit. Running out of rows is not an
// error.
func (r *Rows) Err() error { return r.err }
