package store

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/anshnt/emberdb/internal/btree"
	"github.com/anshnt/emberdb/internal/value"
)

// Table returns a table's definition. The pointer belongs to the transaction:
// changes to it are written back to the catalog when the transaction commits.
func (tx *Tx) Table(name string) (*Table, error) {
	if tx.done {
		return nil, ErrTxDone
	}
	key := strings.ToLower(name)
	if t, ok := tx.tables[key]; ok {
		return t, nil
	}
	encoded, found, err := btree.Get(tx.store(), tx.catalog, []byte(key))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchTable, name)
	}
	t, err := decodeTable(encoded)
	if err != nil {
		return nil, err
	}
	tx.tables[key] = t
	tx.loaded[key] = encoded
	return t, nil
}

// TableNames returns every table's name, in order.
func (tx *Tx) TableNames() ([]string, error) {
	if tx.done {
		return nil, ErrTxDone
	}
	c, err := btree.First(tx.store(), tx.catalog)
	if err != nil {
		return nil, err
	}
	var names []string
	for c.Next() {
		encoded, err := c.Value()
		if err != nil {
			return nil, err
		}
		t, err := decodeTable(encoded)
		if err != nil {
			return nil, err
		}
		names = append(names, t.Name)
	}
	if err := c.Err(); err != nil {
		return nil, err
	}
	// Names cached in this transaction may not be on disk yet.
	for _, t := range tx.tables {
		if !containsFold(names, t.Name) {
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// containsFold reports whether names holds name, case-insensitively.
func containsFold(names []string, name string) bool {
	for _, n := range names {
		if equalFold(n, name) {
			return true
		}
	}
	return false
}

// markDirty records that a table's definition has to be written back.
func (tx *Tx) markDirty(t *Table) {
	tx.tables[strings.ToLower(t.Name)] = t
	tx.schemaChanged = true
}

// flushTables writes every cached table definition whose contents actually
// changed back to the catalog.
//
// Doing it once at commit keeps a million-row insert from rewriting the
// catalog a million times just to advance a row-id counter. Comparing against
// what was read keeps a transaction that only updated or deleted rows from
// logging the catalog page at all, since nothing in the definition moved.
func (tx *Tx) flushTables() error {
	if !tx.schemaChanged {
		return nil
	}
	w, err := tx.write()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(tx.tables))
	for name := range tx.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		encoded := encodeTable(tx.tables[name])
		if previous, ok := tx.loaded[name]; ok && bytes.Equal(previous, encoded) {
			continue
		}
		root, err := btree.Put(w, tx.catalog, []byte(name), encoded)
		if err != nil {
			return err
		}
		tx.catalog = root
		tx.loaded[name] = encoded
	}
	return nil
}

// CreateTable adds a table to the catalog and returns its definition. Columns
// declared PRIMARY KEY or UNIQUE get an index, since that is the only way the
// constraint can be enforced.
func (tx *Tx) CreateTable(name string, columns []Column) (*Table, error) {
	w, err := tx.write()
	if err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("emberdb: table %s has no columns", name)
	}
	for i := range columns {
		for j := i + 1; j < len(columns); j++ {
			if equalFold(columns[i].Name, columns[j].Name) {
				return nil, fmt.Errorf("emberdb: table %s declares column %s twice", name, columns[j].Name)
			}
		}
	}
	if _, err := tx.Table(name); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrTableExists, name)
	} else if !isNoSuchTable(err) {
		return nil, err
	}

	root, err := btree.Create(w)
	if err != nil {
		return nil, fmt.Errorf("emberdb: create table %s: %w", name, err)
	}
	t := &Table{Name: name, Columns: columns, Root: root, NextRowID: 1}
	for i := range t.Columns {
		if t.Columns[i].PrimaryKey {
			t.Columns[i].NotNull = true
		}
		if !t.Columns[i].PrimaryKey && !t.Columns[i].Unique {
			continue
		}
		indexRoot, err := btree.Create(w)
		if err != nil {
			return nil, fmt.Errorf("emberdb: create index on %s.%s: %w", name, t.Columns[i].Name, err)
		}
		t.Indexes = append(t.Indexes, Index{
			Name:   autoIndexName(name, t.Columns[i].Name),
			Column: i,
			Root:   indexRoot,
			Unique: true,
		})
	}
	tx.markDirty(t)
	return t, nil
}

// autoIndexName names the index that backs a PRIMARY KEY or UNIQUE column.
func autoIndexName(table, column string) string {
	return fmt.Sprintf("emberdb_auto_%s_%s", strings.ToLower(table), strings.ToLower(column))
}

// isNoSuchTable reports whether err is a missing-table error.
func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrNoSuchTable.Error())
}

// CreateIndex builds an index over one column of a table and fills it with the
// rows already there.
func (tx *Tx) CreateIndex(t *Table, name string, column int, unique bool) error {
	w, err := tx.write()
	if err != nil {
		return err
	}
	if column < 0 || column >= len(t.Columns) {
		return fmt.Errorf("emberdb: column %d is out of range for table %s", column, t.Name)
	}
	names, err := tx.TableNames()
	if err != nil {
		return err
	}
	for _, tableName := range names {
		other, err := tx.Table(tableName)
		if err != nil {
			return err
		}
		for _, idx := range other.Indexes {
			if equalFold(idx.Name, name) {
				return fmt.Errorf("%w: %s", ErrIndexExists, name)
			}
		}
	}

	root, err := btree.Create(w)
	if err != nil {
		return fmt.Errorf("emberdb: create index %s: %w", name, err)
	}
	index := Index{Name: name, Column: column, Root: root, Unique: unique}
	if err := tx.backfillIndex(t, &index); err != nil {
		return err
	}
	t.Indexes = append(t.Indexes, index)
	tx.markDirty(t)
	return nil
}

// backfillIndex adds an entry for every row version already in the table.
//
// Dead versions are indexed too. Their entries never match a lookup, because
// an index scan re-reads the version it points at and checks it against the
// caller's snapshot, and indexing them keeps the index correct for a
// transaction that started before the index existed.
func (tx *Tx) backfillIndex(t *Table, index *Index) error {
	w, err := tx.write()
	if err != nil {
		return err
	}
	c, err := btree.First(tx.store(), t.Root)
	if err != nil {
		return err
	}
	type entry struct {
		key []byte
	}
	var entries []entry
	seen := make(map[string]bool)
	for c.Next() {
		rowID, xmin, err := parseRowKey(c.Key())
		if err != nil {
			return err
		}
		encoded, err := c.Value()
		if err != nil {
			return err
		}
		xmax, row, err := decodeVersion(encoded, len(t.Columns))
		if err != nil {
			return err
		}
		if index.Unique && tx.snapshot.Visible(xmin) && !tx.snapshot.Visible(xmax) {
			if key := string(value.AppendKey(nil, row[index.Column])); !row[index.Column].IsNull() {
				if seen[key] {
					return fmt.Errorf("%w: index %s would have duplicate values", ErrConstraint, index.Name)
				}
				seen[key] = true
			}
		}
		entries = append(entries, entry{key: indexKey(row[index.Column], rowID, xmin)})
	}
	if err := c.Err(); err != nil {
		return err
	}
	for _, e := range entries {
		root, err := btree.Put(w, index.Root, e.key, nil)
		if err != nil {
			return err
		}
		index.Root = root
	}
	return nil
}
