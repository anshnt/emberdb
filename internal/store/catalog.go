// Package store is emberdb's transactional row engine. It turns the pager, the
// write-ahead log and the B+tree into tables of typed rows with multi-version
// concurrency control, and is everything below the SQL layer.
package store

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/anshnt/emberdb/internal/pager"
	"github.com/anshnt/emberdb/internal/value"
)

// ErrCorruptCatalog reports a catalog entry that does not decode.
var ErrCorruptCatalog = errors.New("emberdb: corrupt catalog entry")

// Column describes one column of a table.
type Column struct {
	// Name is the column's name as written in CREATE TABLE.
	Name string
	// Type is the storage class values in this column are coerced to.
	Type value.Type
	// NotNull rejects null values in this column.
	NotNull bool
	// PrimaryKey marks the column as the table's primary key, which implies
	// NotNull and a unique index.
	PrimaryKey bool
	// Unique requires values in this column to be distinct, enforced
	// through an index.
	Unique bool
}

// Index describes a single-column index.
type Index struct {
	// Name is the index's name, unique across the database.
	Name string
	// Column is the ordinal of the indexed column in its table.
	Column int
	// Root is the page the index's B+tree is rooted at.
	Root pager.PageID
	// Unique makes the index reject a second visible row with the same
	// value.
	Unique bool
}

// Table describes a stored table. A transaction hands out pointers into its
// own cache, so mutations such as a bumped row-id counter accumulate until
// commit writes them back to the catalog.
type Table struct {
	// Name is the table's name.
	Name string
	// Columns are the table's columns in declaration order.
	Columns []Column
	// Root is the page the table's B+tree is rooted at.
	Root pager.PageID
	// NextRowID is the row id the next insert will use.
	NextRowID uint64
	// Indexes are the table's indexes.
	Indexes []Index
}

// ColumnIndex returns the ordinal of a named column, case-insensitively, and
// whether the table has one.
func (t *Table) ColumnIndex(name string) (int, bool) {
	for i := range t.Columns {
		if equalFold(t.Columns[i].Name, name) {
			return i, true
		}
	}
	return 0, false
}

// IndexOn returns an index over the given column ordinal, preferring a unique
// one, and whether the table has any.
func (t *Table) IndexOn(column int) (*Index, bool) {
	var found *Index
	for i := range t.Indexes {
		if t.Indexes[i].Column != column {
			continue
		}
		if t.Indexes[i].Unique {
			return &t.Indexes[i], true
		}
		if found == nil {
			found = &t.Indexes[i]
		}
	}
	return found, found != nil
}

// Column flag bits as stored in the catalog.
const (
	flagNotNull = 1 << iota
	flagPrimaryKey
	flagUnique
)

// encodeTable serialises a table definition for the catalog tree.
func encodeTable(t *Table) []byte {
	out := make([]byte, 0, 128)
	out = appendString(out, t.Name)
	out = binary.AppendUvarint(out, uint64(t.Root))
	out = binary.AppendUvarint(out, t.NextRowID)
	out = binary.AppendUvarint(out, uint64(len(t.Columns)))
	for _, c := range t.Columns {
		out = appendString(out, c.Name)
		out = append(out, byte(c.Type))
		var flags byte
		if c.NotNull {
			flags |= flagNotNull
		}
		if c.PrimaryKey {
			flags |= flagPrimaryKey
		}
		if c.Unique {
			flags |= flagUnique
		}
		out = append(out, flags)
	}
	out = binary.AppendUvarint(out, uint64(len(t.Indexes)))
	for _, idx := range t.Indexes {
		out = appendString(out, idx.Name)
		out = binary.AppendUvarint(out, uint64(idx.Column))
		out = binary.AppendUvarint(out, uint64(idx.Root))
		var flags byte
		if idx.Unique {
			flags |= flagUnique
		}
		out = append(out, flags)
	}
	return out
}

// decodeTable parses a catalog entry.
func decodeTable(src []byte) (*Table, error) {
	d := decoder{src: src}
	t := &Table{}
	t.Name = d.string()
	t.Root = pager.PageID(d.uvarint())
	t.NextRowID = d.uvarint()
	columns := d.uvarint()
	if d.err == nil && columns > 4096 {
		return nil, fmt.Errorf("%w: %d columns", ErrCorruptCatalog, columns)
	}
	for i := uint64(0); i < columns && d.err == nil; i++ {
		var c Column
		c.Name = d.string()
		c.Type = value.Type(d.byte())
		flags := d.byte()
		c.NotNull = flags&flagNotNull != 0
		c.PrimaryKey = flags&flagPrimaryKey != 0
		c.Unique = flags&flagUnique != 0
		t.Columns = append(t.Columns, c)
	}
	indexes := d.uvarint()
	if d.err == nil && indexes > 4096 {
		return nil, fmt.Errorf("%w: %d indexes", ErrCorruptCatalog, indexes)
	}
	for i := uint64(0); i < indexes && d.err == nil; i++ {
		var idx Index
		idx.Name = d.string()
		idx.Column = int(d.uvarint())
		idx.Root = pager.PageID(d.uvarint())
		idx.Unique = d.byte()&flagUnique != 0
		t.Indexes = append(t.Indexes, idx)
	}
	if d.err != nil {
		return nil, d.err
	}
	return t, nil
}

// appendString writes a length-prefixed string.
func appendString(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

// decoder reads the catalog encoding, latching the first error so that callers
// can decode a whole record before checking.
type decoder struct {
	src []byte
	err error
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	n, read := binary.Uvarint(d.src)
	if read <= 0 {
		d.err = fmt.Errorf("%w: unreadable number", ErrCorruptCatalog)
		return 0
	}
	d.src = d.src[read:]
	return n
}

func (d *decoder) byte() byte {
	if d.err != nil {
		return 0
	}
	if len(d.src) == 0 {
		d.err = fmt.Errorf("%w: entry ended early", ErrCorruptCatalog)
		return 0
	}
	b := d.src[0]
	d.src = d.src[1:]
	return b
}

func (d *decoder) string() string {
	length := d.uvarint()
	if d.err != nil {
		return ""
	}
	if int(length) > len(d.src) {
		d.err = fmt.Errorf("%w: string claims %d bytes, %d remain", ErrCorruptCatalog, length, len(d.src))
		return ""
	}
	s := string(d.src[:length])
	d.src = d.src[length:]
	return s
}

// equalFold compares identifiers case-insensitively, which is how SQL names
// behave.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lower(a[i]) != lower(b[i]) {
			return false
		}
	}
	return true
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
