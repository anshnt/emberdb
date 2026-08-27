package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/anshnt/emberdb/internal/value"
)

// open creates a database on a temporary file and closes it when the test ends.
func open(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.ember")
	db := openAt(t, path)
	return db, path
}

// openAt opens a database at a given path.
func openAt(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path, Options{NoSync: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// people is a small table definition used across the tests.
func people() []Column {
	return []Column{
		{Name: "name", Type: value.TypeText, NotNull: true},
		{Name: "age", Type: value.TypeInteger},
	}
}

// createPeople makes the people table and returns nothing; the definition is
// re-read per transaction.
func createPeople(t *testing.T, db *DB) {
	t.Helper()
	err := db.Update(func(tx *Tx) error {
		_, err := tx.CreateTable("people", people())
		return err
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
}

// insert adds a row and returns its id.
func insert(t *testing.T, db *DB, table string, values ...value.Value) uint64 {
	t.Helper()
	var id uint64
	err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table(table)
		if err != nil {
			return err
		}
		id, err = tx.Insert(tbl, values)
		return err
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return id
}

// collect returns every visible row of a table, as strings.
func collect(t *testing.T, tx *Tx, table string) []string {
	t.Helper()
	tbl, err := tx.Table(table)
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	rows, err := tx.Scan(tbl)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var out []string
	for rows.Next() {
		r := rows.Row()
		out = append(out, fmt.Sprintf("%d:%s", r.ID, joinValues(r.Values)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return out
}

// joinValues renders a row for comparison in tests.
func joinValues(values []value.Value) string {
	out := ""
	for i, v := range values {
		if i > 0 {
			out += ","
		}
		out += v.String()
	}
	return out
}

func TestCreateTableAndInsert(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	insert(t, db, "people", value.Text("ada"), value.Integer(36))
	insert(t, db, "people", value.Text("grace"), value.Integer(45))

	err := db.View(func(tx *Tx) error {
		got := collect(t, tx, "people")
		want := []string{"1:ada,36", "2:grace,45"}
		if len(got) != len(want) {
			t.Fatalf("scan returned %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("row %d is %q, want %q", i, got[i], want[i])
			}
		}
		names, err := tx.TableNames()
		if err != nil {
			return err
		}
		if len(names) != 1 || names[0] != "people" {
			t.Fatalf("TableNames = %v", names)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestCreateTableRejectsDuplicates(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	err := db.Update(func(tx *Tx) error {
		_, err := tx.CreateTable("PEOPLE", people())
		return err
	})
	if !errors.Is(err, ErrTableExists) {
		t.Fatalf("CreateTable = %v, want ErrTableExists (names are case-insensitive)", err)
	}
	err = db.Update(func(tx *Tx) error {
		_, err := tx.CreateTable("dupes", []Column{
			{Name: "a", Type: value.TypeInteger},
			{Name: "A", Type: value.TypeText},
		})
		return err
	})
	if err == nil {
		t.Fatal("CreateTable accepted two columns with the same name")
	}
}

func TestReadYourWrites(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("people")
		if err != nil {
			return err
		}
		id, err := tx.Insert(tbl, []value.Value{value.Text("ada"), value.Integer(36)})
		if err != nil {
			return err
		}
		// The row is readable inside the transaction that made it.
		row, found, err := tx.Get(tbl, id)
		if err != nil {
			return err
		}
		if !found || row.Values[0].Str() != "ada" {
			t.Fatalf("Get after Insert = (%v, %v)", row, found)
		}
		if got := collect(t, tx, "people"); len(got) != 1 || got[0] != "1:ada,36" {
			t.Fatalf("scan inside the writing transaction = %v", got)
		}
		// So is an update to it.
		if _, err := tx.Update(tbl, id, []value.Value{value.Text("ada l"), value.Integer(37)}); err != nil {
			return err
		}
		if got := collect(t, tx, "people"); len(got) != 1 || got[0] != "1:ada l,37" {
			t.Fatalf("scan after update = %v", got)
		}
		// And a delete.
		if _, err := tx.Delete(tbl, id); err != nil {
			return err
		}
		if got := collect(t, tx, "people"); len(got) != 0 {
			t.Fatalf("scan after delete = %v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestSnapshotIsolation(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	insert(t, db, "people", value.Text("ada"), value.Integer(36))

	// A reader opened now must keep seeing the database as it is, whatever
	// happens next.
	reader, err := db.Begin(false)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer reader.Rollback()
	before := collect(t, reader, "people")

	// A writer runs to completion while the reader is open. If the reader
	// held a lock the writer needed, this would deadlock rather than fail.
	insert(t, db, "people", value.Text("grace"), value.Integer(45))
	if err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("people")
		if err != nil {
			return err
		}
		_, err = tx.Update(tbl, 1, []value.Value{value.Text("changed"), value.Integer(99)})
		return err
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	after := collect(t, reader, "people")
	if len(after) != len(before) {
		t.Fatalf("reader saw %v, was %v: a committed transaction leaked into an open snapshot", after, before)
	}
	if len(after) != 1 || after[0] != "1:ada,36" {
		t.Fatalf("reader sees %v, want the state it began with", after)
	}

	// A reader opened now sees everything.
	if err := db.View(func(tx *Tx) error {
		got := collect(t, tx, "people")
		if len(got) != 2 || got[0] != "1:changed,99" || got[1] != "2:grace,45" {
			t.Fatalf("fresh reader sees %v", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestSnapshotIsolationAcrossALongScan(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	const rows = 2000
	if err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("people")
		if err != nil {
			return err
		}
		for i := 0; i < rows; i++ {
			if _, err := tx.Insert(tbl, []value.Value{value.Text(fmt.Sprintf("p%05d", i)), value.Integer(int64(i))}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	reader, err := db.Begin(false)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer reader.Rollback()
	tbl, err := reader.Table("people")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	iter, err := reader.Scan(tbl)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// Read a few rows, then let a writer churn the tree hard enough to
	// split and merge pages under the open cursor.
	seen := 0
	for seen < 5 && iter.Next() {
		seen++
	}
	if err := db.Update(func(tx *Tx) error {
		w, err := tx.Table("people")
		if err != nil {
			return err
		}
		for i := uint64(1); i <= rows; i += 2 {
			if _, err := tx.Delete(w, i); err != nil {
				return err
			}
		}
		for i := 0; i < 500; i++ {
			if _, err := tx.Insert(w, []value.Value{value.Text(fmt.Sprintf("new%05d", i)), value.Integer(-1)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("concurrent writer: %v", err)
	}

	for iter.Next() {
		seen++
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if seen != rows {
		t.Fatalf("the scan saw %d rows, want %d: a concurrent commit disturbed an open snapshot", seen, rows)
	}
}

func TestRollbackLeavesNoTrace(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	insert(t, db, "people", value.Text("ada"), value.Integer(36))
	sizeBefore := db.Stats().Pages

	tx, err := db.Begin(true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tbl, err := tx.Table("people")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	for i := 0; i < 500; i++ {
		if _, err := tx.Insert(tbl, []value.Value{value.Text(fmt.Sprintf("ghost%d", i)), value.Integer(int64(i))}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	if _, err := tx.Update(tbl, 1, []value.Value{value.Text("wrong"), value.Integer(0)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := tx.Delete(tbl, 1); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if err := db.View(func(tx *Tx) error {
		got := collect(t, tx, "people")
		if len(got) != 1 || got[0] != "1:ada,36" {
			t.Fatalf("after rollback the table holds %v", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
	if got := db.Stats().Pages; got != sizeBefore {
		t.Fatalf("a rolled back transaction grew the file from %d to %d pages", sizeBefore, got)
	}
	// The next row id must not have been consumed by the rollback either.
	if id := insert(t, db, "people", value.Text("grace"), value.Integer(45)); id != 2 {
		t.Fatalf("row id after rollback = %d, want 2", id)
	}
}

func TestRollbackOnErrorInsideUpdate(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	sentinel := errors.New("deliberate")
	err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("people")
		if err != nil {
			return err
		}
		if _, err := tx.Insert(tbl, []value.Value{value.Text("ada"), value.Integer(36)}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Update = %v, want the callback's error", err)
	}
	if err := db.View(func(tx *Tx) error {
		if got := collect(t, tx, "people"); len(got) != 0 {
			t.Fatalf("the failed transaction left %v behind", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestDataSurvivesReopen(t *testing.T) {
	db, path := open(t)
	createPeople(t, db)
	for i := 0; i < 300; i++ {
		insert(t, db, "people", value.Text(fmt.Sprintf("p%03d", i)), value.Integer(int64(i)))
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openAt(t, path)
	if err := reopened.View(func(tx *Tx) error {
		got := collect(t, tx, "people")
		if len(got) != 300 {
			t.Fatalf("after reopen the table holds %d rows, want 300", len(got))
		}
		if got[0] != "1:p000,0" || got[299] != "300:p299,299" {
			t.Fatalf("rows came back as %q ... %q", got[0], got[299])
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestUncleanReopenReplaysTheLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unclean.ember")
	db, err := Open(path, Options{NoSync: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	createPeople(t, db)
	for i := 0; i < 200; i++ {
		insert(t, db, "people", value.Text(fmt.Sprintf("p%03d", i)), value.Integer(int64(i)))
	}
	// Walk away without closing: the log is left in place, exactly as a
	// killed process would leave it.

	reopened := openAt(t, path)
	if err := reopened.View(func(tx *Tx) error {
		if got := collect(t, tx, "people"); len(got) != 200 {
			t.Fatalf("after replaying the log the table holds %d rows, want 200", len(got))
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestNotNullAndTypeRules(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("people")
		if err != nil {
			return err
		}
		_, err = tx.Insert(tbl, []value.Value{value.Null(), value.Integer(1)})
		return err
	})
	if !errors.Is(err, ErrConstraint) {
		t.Fatalf("inserting NULL into a NOT NULL column = %v, want ErrConstraint", err)
	}

	err = db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("people")
		if err != nil {
			return err
		}
		_, err = tx.Insert(tbl, []value.Value{value.Text("ada"), value.Text("thirty")})
		return err
	})
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("inserting TEXT into an INTEGER column = %v, want ErrTypeMismatch", err)
	}

	// An integer widens into a REAL column, and nothing else converts.
	if err := db.Update(func(tx *Tx) error {
		tbl, err := tx.CreateTable("measurements", []Column{{Name: "v", Type: value.TypeReal}})
		if err != nil {
			return err
		}
		if _, err := tx.Insert(tbl, []value.Value{value.Integer(3)}); err != nil {
			return err
		}
		row, _, err := tx.Get(tbl, 1)
		if err != nil {
			return err
		}
		if row.Values[0].Kind() != value.TypeReal || row.Values[0].Float() != 3 {
			t.Fatalf("integer stored in a REAL column came back as %v (%s)", row.Values[0], row.Values[0].Kind())
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestUniqueConstraint(t *testing.T) {
	db, _ := open(t)
	if err := db.Update(func(tx *Tx) error {
		_, err := tx.CreateTable("users", []Column{
			{Name: "email", Type: value.TypeText, Unique: true},
			{Name: "name", Type: value.TypeText},
		})
		return err
	}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	insert(t, db, "users", value.Text("a@example.com"), value.Text("ada"))

	err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("users")
		if err != nil {
			return err
		}
		_, err = tx.Insert(tbl, []value.Value{value.Text("a@example.com"), value.Text("someone else")})
		return err
	})
	if !errors.Is(err, ErrConstraint) {
		t.Fatalf("duplicate insert = %v, want ErrConstraint", err)
	}

	// Nulls do not conflict with each other.
	if err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("users")
		if err != nil {
			return err
		}
		for i := 0; i < 3; i++ {
			if _, err := tx.Insert(tbl, []value.Value{value.Null(), value.Text("anon")}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("null values must not collide in a unique index: %v", err)
	}

	// Deleting the row frees the value again.
	if err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("users")
		if err != nil {
			return err
		}
		if _, err := tx.Delete(tbl, 1); err != nil {
			return err
		}
		_, err = tx.Insert(tbl, []value.Value{value.Text("a@example.com"), value.Text("reused")})
		return err
	}); err != nil {
		t.Fatalf("reusing a deleted unique value: %v", err)
	}
}

func TestConcurrentReadersDuringWrites(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	for i := 0; i < 100; i++ {
		insert(t, db, "people", value.Text(fmt.Sprintf("p%03d", i)), value.Integer(int64(i)))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 8)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				err := db.View(func(tx *Tx) error {
					tbl, err := tx.Table("people")
					if err != nil {
						return err
					}
					rows, err := tx.Scan(tbl)
					if err != nil {
						return err
					}
					count := 0
					for rows.Next() {
						if len(rows.Row().Values) != 2 {
							return fmt.Errorf("row %d has %d values", rows.Row().ID, len(rows.Row().Values))
						}
						count++
					}
					if err := rows.Err(); err != nil {
						return err
					}
					if count < 100 {
						return fmt.Errorf("a reader saw %d rows, fewer than the 100 committed before it started", count)
					}
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		if err := db.Update(func(tx *Tx) error {
			tbl, err := tx.Table("people")
			if err != nil {
				return err
			}
			_, err = tx.Insert(tbl, []value.Value{value.Text(fmt.Sprintf("w%03d", i)), value.Integer(int64(i))})
			return err
		}); err != nil {
			t.Fatalf("writer: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("reader: %v", err)
	}
}

func TestTransactionUseAfterFinish(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	tx, err := db.Begin(true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := tx.Table("people"); !errors.Is(err, ErrTxDone) {
		t.Fatalf("Table after Rollback = %v, want ErrTxDone", err)
	}
	if err := tx.Commit(); !errors.Is(err, ErrTxDone) {
		t.Fatalf("Commit after Rollback = %v, want ErrTxDone", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("a second Rollback should be a no-op, got %v", err)
	}
}

func TestReadOnlyTransactionsCannotWrite(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	err := db.View(func(tx *Tx) error {
		tbl, err := tx.Table("people")
		if err != nil {
			return err
		}
		_, err = tx.Insert(tbl, []value.Value{value.Text("nope"), value.Integer(0)})
		return err
	})
	if !errors.Is(err, ErrReadOnlyTx) {
		t.Fatalf("Insert in a read transaction = %v, want ErrReadOnlyTx", err)
	}
}
