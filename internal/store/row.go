package store

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/anshnt/emberdb/internal/btree"
	"github.com/anshnt/emberdb/internal/value"
)

// ErrCorruptRow reports a stored row that does not decode.
var ErrCorruptRow = errors.New("emberdb: corrupt row")

// A table's B+tree holds one entry per row version. The key is the row id
// followed by the id of the transaction that created the version, both
// big-endian so that byte order matches numeric order and every version of a
// row sits together, oldest first:
//
//	0  8  row id
//	8  8  creating transaction id (xmin)
//
// The value carries the transaction that deleted the version, or zero while it
// is live, followed by the encoded row:
//
//	0  8  deleting transaction id (xmax)
//	8  .. the row's values
const (
	rowKeySize     = 16
	versionHeader  = 8
	rowKeyIDOffset = 0
	rowKeyTxOffset = 8
)

// rowKey builds the key for one version of a row.
func rowKey(rowID, xmin uint64) []byte {
	key := make([]byte, rowKeySize)
	binary.BigEndian.PutUint64(key[rowKeyIDOffset:], rowID)
	binary.BigEndian.PutUint64(key[rowKeyTxOffset:], xmin)
	return key
}

// parseRowKey splits a table key back into its row id and creating
// transaction.
func parseRowKey(key []byte) (rowID, xmin uint64, err error) {
	if len(key) != rowKeySize {
		return 0, 0, fmt.Errorf("%w: table key is %d bytes, want %d", ErrCorruptRow, len(key), rowKeySize)
	}
	return binary.BigEndian.Uint64(key[rowKeyIDOffset:]), binary.BigEndian.Uint64(key[rowKeyTxOffset:]), nil
}

// encodeVersion builds the stored form of a row version.
func encodeVersion(xmax uint64, row []value.Value) []byte {
	out := make([]byte, versionHeader, versionHeader+len(row)*8)
	binary.LittleEndian.PutUint64(out, xmax)
	return value.AppendRow(out, row)
}

// decodeVersion reads a stored row version.
func decodeVersion(data []byte, columns int) (xmax uint64, row []value.Value, err error) {
	if len(data) < versionHeader {
		return 0, nil, fmt.Errorf("%w: version is %d bytes", ErrCorruptRow, len(data))
	}
	row, err = value.DecodeRow(data[versionHeader:], columns)
	if err != nil {
		return 0, nil, err
	}
	return binary.LittleEndian.Uint64(data), row, nil
}

// Row is a row as a caller sees it.
type Row struct {
	// ID is the row's identity, stable across updates.
	ID uint64
	// Values are the row's column values, in table order.
	Values []value.Value
}

// version is one stored row version.
type version struct {
	rowID uint64
	xmin  uint64
	xmax  uint64
	row   []value.Value
}

// visible reports whether this version is the one a snapshot should see.
func (v version) visible(s Snapshot) bool {
	return s.Visible(v.xmin) && !s.Visible(v.xmax)
}

// versions returns every stored version of a row, oldest first.
func (tx *Tx) versions(t *Table, rowID uint64) ([]version, error) {
	c, err := btree.Seek(tx.store(), t.Root, rowKey(rowID, 0))
	if err != nil {
		return nil, err
	}
	var out []version
	for c.Next() {
		id, xmin, err := parseRowKey(c.Key())
		if err != nil {
			return nil, err
		}
		if id != rowID {
			break
		}
		encoded, err := c.Value()
		if err != nil {
			return nil, err
		}
		xmax, row, err := decodeVersion(encoded, len(t.Columns))
		if err != nil {
			return nil, err
		}
		out = append(out, version{rowID: id, xmin: xmin, xmax: xmax, row: row})
	}
	if err := c.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// visibleVersion returns the version of a row this transaction can see.
func (tx *Tx) visibleVersion(t *Table, rowID uint64) (version, bool, error) {
	all, err := tx.versions(t, rowID)
	if err != nil {
		return version{}, false, err
	}
	for _, v := range all {
		if v.visible(tx.snapshot) {
			return v, true, nil
		}
	}
	return version{}, false, nil
}

// Get returns a row by id, if the transaction can see it.
func (tx *Tx) Get(t *Table, rowID uint64) (Row, bool, error) {
	if tx.done {
		return Row{}, false, ErrTxDone
	}
	v, found, err := tx.visibleVersion(t, rowID)
	if err != nil || !found {
		return Row{}, false, err
	}
	return Row{ID: v.rowID, Values: v.row}, true, nil
}

// coerce fits a value to a column's declared type, or reports why it does not.
//
// The rules are deliberately narrow: an integer widens into a REAL column, and
// nothing else converts. Keeping a column to a single storage class is what
// lets an index's byte order match the column's value order, and it means a
// query never has to reason about a column that holds both 7 and "seven".
func coerce(c Column, v value.Value) (value.Value, error) {
	if v.IsNull() {
		if c.NotNull {
			return v, fmt.Errorf("%w: column %s is NOT NULL", ErrConstraint, c.Name)
		}
		return v, nil
	}
	if v.Kind() == c.Type {
		return v, nil
	}
	if c.Type == value.TypeReal && v.Kind() == value.TypeInteger {
		return value.Real(float64(v.Int())), nil
	}
	return v, fmt.Errorf("%w: column %s is %s, cannot store %s", ErrTypeMismatch, c.Name, c.Type, v.Kind())
}

// coerceRow fits a whole row to a table's columns.
func coerceRow(t *Table, row []value.Value) ([]value.Value, error) {
	if len(row) != len(t.Columns) {
		return nil, fmt.Errorf("emberdb: table %s has %d columns, got %d values", t.Name, len(t.Columns), len(row))
	}
	out := make([]value.Value, len(row))
	for i := range row {
		v, err := coerce(t.Columns[i], row[i])
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// Insert stores a new row and returns the id it was given.
func (tx *Tx) Insert(t *Table, row []value.Value) (uint64, error) {
	w, err := tx.write()
	if err != nil {
		return 0, err
	}
	stored, err := coerceRow(t, row)
	if err != nil {
		return 0, err
	}
	rowID := t.NextRowID
	if err := tx.checkUnique(t, stored, rowID); err != nil {
		return 0, err
	}
	root, err := btree.Put(w, t.Root, rowKey(rowID, tx.id), encodeVersion(0, stored))
	if err != nil {
		return 0, err
	}
	t.Root = root
	t.NextRowID++
	if err := tx.addIndexEntries(t, stored, rowID, tx.id); err != nil {
		return 0, err
	}
	tx.markDirty(t)
	return rowID, nil
}

// Update replaces a row's values, leaving the previous version in place for
// transactions whose snapshot still includes it.
func (tx *Tx) Update(t *Table, rowID uint64, row []value.Value) (bool, error) {
	w, err := tx.write()
	if err != nil {
		return false, err
	}
	current, found, err := tx.visibleVersion(t, rowID)
	if err != nil || !found {
		return false, err
	}
	stored, err := coerceRow(t, row)
	if err != nil {
		return false, err
	}
	if err := tx.checkUnique(t, stored, rowID); err != nil {
		return false, err
	}
	if current.xmin != tx.id {
		// Mark the old version as deleted by this transaction. When the
		// transaction created it, the write below replaces it outright
		// and there is nothing to mark.
		root, err := btree.Put(w, t.Root, rowKey(rowID, current.xmin), encodeVersion(tx.id, current.row))
		if err != nil {
			return false, err
		}
		t.Root = root
	}
	root, err := btree.Put(w, t.Root, rowKey(rowID, tx.id), encodeVersion(0, stored))
	if err != nil {
		return false, err
	}
	t.Root = root
	if err := tx.addIndexEntries(t, stored, rowID, tx.id); err != nil {
		return false, err
	}
	tx.markDirty(t)
	return true, tx.prune(t, rowID)
}

// Delete marks a row deleted by this transaction. The version stays until no
// snapshot can still see it.
func (tx *Tx) Delete(t *Table, rowID uint64) (bool, error) {
	w, err := tx.write()
	if err != nil {
		return false, err
	}
	current, found, err := tx.visibleVersion(t, rowID)
	if err != nil || !found {
		return false, err
	}
	root, err := btree.Put(w, t.Root, rowKey(rowID, current.xmin), encodeVersion(tx.id, current.row))
	if err != nil {
		return false, err
	}
	t.Root = root
	tx.markDirty(t)
	return true, tx.prune(t, rowID)
}

// prune removes versions of a row that no open or future transaction can see,
// along with their index entries.
//
// This is emberdb's whole garbage collection story: a row is tidied when it is
// next written, which bounds the versions a repeatedly updated row accumulates
// without needing a background vacuum.
func (tx *Tx) prune(t *Table, rowID uint64) error {
	w, err := tx.write()
	if err != nil {
		return err
	}
	oldest := tx.db.oldestSnapshot()
	all, err := tx.versions(t, rowID)
	if err != nil {
		return err
	}
	for _, v := range all {
		if v.xmax == 0 || v.xmax > oldest || v.xmax == tx.id {
			continue
		}
		root, _, err := btree.Delete(w, t.Root, rowKey(v.rowID, v.xmin))
		if err != nil {
			return err
		}
		t.Root = root
		if err := tx.removeIndexEntries(t, v.row, v.rowID, v.xmin); err != nil {
			return err
		}
		tx.markDirty(t)
	}
	return nil
}
