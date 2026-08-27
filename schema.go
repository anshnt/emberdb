package emberdb

import (
	"fmt"
	"sort"
	"strings"

	"github.com/anshnt/emberdb/internal/store"
)

// ColumnInfo describes one column of a table.
type ColumnInfo struct {
	// Name is the column's name.
	Name string
	// Type is its declared storage class.
	Type Type
	// NotNull reports whether nulls are rejected.
	NotNull bool
	// PrimaryKey reports whether the column is the table's primary key.
	PrimaryKey bool
	// Unique reports whether values must be distinct.
	Unique bool
}

// IndexInfo describes one index.
type IndexInfo struct {
	// Name is the index's name.
	Name string
	// Column is the column it covers.
	Column string
	// Unique reports whether it rejects duplicates.
	Unique bool
	// Implicit reports whether emberdb created it to enforce a PRIMARY KEY
	// or UNIQUE column rather than the user asking for it.
	Implicit bool
}

// TableInfo describes a table.
type TableInfo struct {
	// Name is the table's name.
	Name string
	// Columns are its columns, in declaration order.
	Columns []ColumnInfo
	// Indexes are the indexes over it.
	Indexes []IndexInfo
	// Rows is the number of rows visible right now.
	Rows int
}

// Tables returns the names of every table, in order.
func (db *DB) Tables() ([]string, error) {
	var names []string
	err := db.store.View(func(tx *store.Tx) error {
		var err error
		names, err = tx.TableNames()
		return err
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// TableInfo describes one table, including how many rows it currently holds.
func (db *DB) TableInfo(name string) (*TableInfo, error) {
	var info *TableInfo
	err := db.store.View(func(tx *store.Tx) error {
		table, err := tx.Table(name)
		if err != nil {
			return err
		}
		info = describe(table)
		rows, err := tx.Scan(table)
		if err != nil {
			return err
		}
		for rows.Next() {
			info.Rows++
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// describe converts a stored table definition into its public description.
func describe(table *store.Table) *TableInfo {
	info := &TableInfo{Name: table.Name}
	for _, c := range table.Columns {
		info.Columns = append(info.Columns, ColumnInfo{
			Name:       c.Name,
			Type:       c.Type,
			NotNull:    c.NotNull,
			PrimaryKey: c.PrimaryKey,
			Unique:     c.Unique,
		})
	}
	for _, index := range table.Indexes {
		info.Indexes = append(info.Indexes, IndexInfo{
			Name:     index.Name,
			Column:   table.Columns[index.Column].Name,
			Unique:   index.Unique,
			Implicit: strings.HasPrefix(index.Name, "emberdb_auto_"),
		})
	}
	return info
}

// DDL reconstructs the CREATE statements for a table, which is what the CLI's
// .schema prints.
func (info *TableInfo) DDL() string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", info.Name)
	for i, c := range info.Columns {
		fmt.Fprintf(&b, "  %s %s", c.Name, c.Type)
		if c.PrimaryKey {
			b.WriteString(" PRIMARY KEY")
		} else {
			if c.Unique {
				b.WriteString(" UNIQUE")
			}
			if c.NotNull {
				b.WriteString(" NOT NULL")
			}
		}
		if i < len(info.Columns)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(");")
	for _, index := range info.Indexes {
		if index.Implicit {
			continue
		}
		b.WriteString("\n")
		if index.Unique {
			fmt.Fprintf(&b, "CREATE UNIQUE INDEX %s ON %s (%s);", index.Name, info.Name, index.Column)
			continue
		}
		fmt.Fprintf(&b, "CREATE INDEX %s ON %s (%s);", index.Name, info.Name, index.Column)
	}
	return b.String()
}

// Stats describes the database's current size and activity.
type Stats struct {
	// Pages is how many 4 KiB pages the file holds.
	Pages uint32
	// FreePages is how many of them are free for reuse.
	FreePages uint32
	// CachedPages is how many pages are held in memory.
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
	s := db.store.Stats()
	return Stats{
		Pages:        s.Pages,
		FreePages:    s.FreePages,
		CachedPages:  s.CachedPages,
		PendingPages: s.PendingPages,
		LogBytes:     s.LogBytes,
		Syncs:        s.Syncs,
		LastTxID:     s.LastTxID,
	}
}

// Checkpoint folds the write-ahead log into the database file. It happens
// automatically as the log grows; this is for callers who want to control when.
func (db *DB) Checkpoint() error { return db.store.Checkpoint() }

// FileSize returns the size of the database file in bytes.
func (db *DB) FileSize() (int64, error) { return db.store.FileSize() }
