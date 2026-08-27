package store

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/anshnt/emberdb/internal/value"
)

// scores creates a table with an indexed integer column and fills it.
func scores(t *testing.T, db *DB, values []int64, unique bool) {
	t.Helper()
	err := db.Update(func(tx *Tx) error {
		tbl, err := tx.CreateTable("scores", []Column{
			{Name: "label", Type: value.TypeText},
			{Name: "score", Type: value.TypeInteger},
		})
		if err != nil {
			return err
		}
		for i, v := range values {
			if _, err := tx.Insert(tbl, []value.Value{value.Text(fmt.Sprintf("row%03d", i)), value.Integer(v)}); err != nil {
				return err
			}
		}
		return tx.CreateIndex(tbl, "scores_by_score", 1, unique)
	})
	if err != nil {
		t.Fatalf("build scores: %v", err)
	}
}

// indexed returns the row ids an index scan yields for a range.
func indexed(t *testing.T, tx *Tx, rng Range) []uint64 {
	t.Helper()
	tbl, err := tx.Table("scores")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	index, ok := tbl.IndexOn(1)
	if !ok {
		t.Fatal("table has no index on column 1")
	}
	rows, err := tx.ScanIndex(tbl, index, rng)
	if err != nil {
		t.Fatalf("ScanIndex: %v", err)
	}
	var ids []uint64
	for rows.Next() {
		ids = append(ids, rows.Row().ID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ScanIndex: %v", err)
	}
	return ids
}

// filtered returns the row ids a full scan yields for the same range, which is
// the answer the index has to agree with.
func filtered(t *testing.T, tx *Tx, rng Range) []uint64 {
	t.Helper()
	tbl, err := tx.Table("scores")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	rows, err := tx.Scan(tbl)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var ids []uint64
	for rows.Next() {
		v := rows.Row().Values[1]
		if rng.Low != nil {
			c := value.Compare(v, *rng.Low)
			if c < 0 || (c == 0 && rng.LowOpen) {
				continue
			}
		}
		if rng.High != nil {
			c := value.Compare(v, *rng.High)
			if c > 0 || (c == 0 && rng.HighOpen) {
				continue
			}
		}
		ids = append(ids, rows.Row().ID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func TestIndexScanAgreesWithFullScan(t *testing.T) {
	db, _ := open(t)
	r := rand.New(rand.NewSource(11))
	values := make([]int64, 600)
	for i := range values {
		values[i] = int64(r.Intn(200) - 100)
	}
	scores(t, db, values, false)

	bounds := []int64{-150, -100, -50, -1, 0, 1, 50, 99, 100, 150}
	if err := db.View(func(tx *Tx) error {
		for _, low := range bounds {
			for _, high := range bounds {
				if high < low {
					continue
				}
				for _, open := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
					lo, hi := value.Integer(low), value.Integer(high)
					rng := Range{Low: &lo, High: &hi, LowOpen: open[0], HighOpen: open[1]}
					got := indexed(t, tx, rng)
					sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
					want := filtered(t, tx, rng)
					if len(got) != len(want) {
						t.Fatalf("range [%d,%d] open=%v: index returned %d rows, scan returned %d", low, high, open, len(got), len(want))
					}
					for i := range want {
						if got[i] != want[i] {
							t.Fatalf("range [%d,%d]: index and scan disagree at %d", low, high, i)
						}
					}
				}
			}
		}
		// Unbounded on each side.
		lo := value.Integer(0)
		if got, want := len(indexed(t, tx, Range{Low: &lo})), len(filtered(t, tx, Range{Low: &lo})); got != want {
			t.Fatalf("half-open range returned %d rows, want %d", got, want)
		}
		if got, want := len(indexed(t, tx, Range{})), len(filtered(t, tx, Range{})); got != want {
			t.Fatalf("unbounded range returned %d rows, want %d", got, want)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestIndexScanReturnsEachRowOnce(t *testing.T) {
	db, _ := open(t)
	scores(t, db, []int64{5, 5, 5, 7}, false)

	// Update the same row twice inside one transaction. The first write
	// leaves an index entry under the same row and transaction id as the
	// second, and the scan has to discard it.
	if err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("scores")
		if err != nil {
			return err
		}
		if _, err := tx.Update(tbl, 1, []value.Value{value.Text("row000"), value.Integer(11)}); err != nil {
			return err
		}
		_, err = tx.Update(tbl, 1, []value.Value{value.Text("row000"), value.Integer(12)})
		return err
	}); err != nil {
		t.Fatalf("double update: %v", err)
	}

	if err := db.View(func(tx *Tx) error {
		lo, hi := value.Integer(-1000), value.Integer(1000)
		ids := indexed(t, tx, Range{Low: &lo, High: &hi})
		seen := make(map[uint64]int)
		for _, id := range ids {
			seen[id]++
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("row %d came back %d times from the index", id, n)
			}
		}
		if len(ids) != 4 {
			t.Fatalf("index returned %d rows, want 4", len(ids))
		}
		// The stale value must not be reachable, and the current one must.
		eleven := value.Integer(11)
		if got := indexed(t, tx, Range{Low: &eleven, High: &eleven}); len(got) != 0 {
			t.Fatalf("the superseded value 11 still matches rows %v", got)
		}
		twelve := value.Integer(12)
		if got := indexed(t, tx, Range{Low: &twelve, High: &twelve}); len(got) != 1 || got[0] != 1 {
			t.Fatalf("looking up the current value 12 returned %v", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestIndexRespectsSnapshots(t *testing.T) {
	db, _ := open(t)
	scores(t, db, []int64{10, 20, 30}, false)

	reader, err := db.Begin(false)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer reader.Rollback()

	if err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("scores")
		if err != nil {
			return err
		}
		if _, err := tx.Update(tbl, 2, []value.Value{value.Text("row001"), value.Integer(99)}); err != nil {
			return err
		}
		_, err = tx.Insert(tbl, []value.Value{value.Text("new"), value.Integer(20)})
		return err
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	twenty := value.Integer(20)
	if got := indexed(t, reader, Range{Low: &twenty, High: &twenty}); len(got) != 1 || got[0] != 2 {
		t.Fatalf("the open snapshot looks up 20 and gets %v, want just row 2", got)
	}
	ninetyNine := value.Integer(99)
	if got := indexed(t, reader, Range{Low: &ninetyNine, High: &ninetyNine}); len(got) != 0 {
		t.Fatalf("the open snapshot can see the value 99 written after it began: %v", got)
	}
}

func TestIndexSurvivesDeleteAndReopen(t *testing.T) {
	db, path := open(t)
	values := make([]int64, 400)
	for i := range values {
		values[i] = int64(i)
	}
	scores(t, db, values, true)
	if err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("scores")
		if err != nil {
			return err
		}
		for id := uint64(1); id <= 400; id += 2 {
			if _, err := tx.Delete(tbl, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openAt(t, path)
	if err := reopened.View(func(tx *Tx) error {
		lo, hi := value.Integer(0), value.Integer(1000)
		got := indexed(t, tx, Range{Low: &lo, High: &hi})
		if len(got) != 200 {
			t.Fatalf("after deleting half the rows the index returns %d, want 200", len(got))
		}
		for _, id := range got {
			if id%2 == 1 {
				t.Fatalf("the index still returns deleted row %d", id)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestCreateIndexRejectsDuplicateNamesAndValues(t *testing.T) {
	db, _ := open(t)
	scores(t, db, []int64{1, 2, 3}, false)
	err := db.Update(func(tx *Tx) error {
		tbl, err := tx.Table("scores")
		if err != nil {
			return err
		}
		return tx.CreateIndex(tbl, "SCORES_BY_SCORE", 0, false)
	})
	if !errors.Is(err, ErrIndexExists) {
		t.Fatalf("duplicate index name = %v, want ErrIndexExists", err)
	}

	db2, _ := open(t)
	scores(t, db2, []int64{4, 4, 5}, false)
	err = db2.Update(func(tx *Tx) error {
		tbl, err := tx.Table("scores")
		if err != nil {
			return err
		}
		return tx.CreateIndex(tbl, "unique_scores", 1, true)
	})
	if !errors.Is(err, ErrConstraint) {
		t.Fatalf("unique index over duplicate values = %v, want ErrConstraint", err)
	}
}

func TestVersionsArePrunedOnRewrite(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	insert(t, db, "people", value.Text("ada"), value.Integer(0))

	// Rewriting one row a thousand times must not leave a thousand versions
	// behind when nothing is reading them.
	for i := 1; i <= 1000; i++ {
		if err := db.Update(func(tx *Tx) error {
			tbl, err := tx.Table("people")
			if err != nil {
				return err
			}
			_, err = tx.Update(tbl, 1, []value.Value{value.Text("ada"), value.Integer(int64(i))})
			return err
		}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	if err := db.View(func(tx *Tx) error {
		tbl, err := tx.Table("people")
		if err != nil {
			return err
		}
		all, err := tx.versions(tbl, 1)
		if err != nil {
			return err
		}
		if len(all) > 3 {
			t.Fatalf("row 1 has %d stored versions after 1000 updates", len(all))
		}
		row, found, err := tx.Get(tbl, 1)
		if err != nil || !found {
			t.Fatalf("Get = (%v, %v, %v)", row, found, err)
		}
		if row.Values[1].Int() != 1000 {
			t.Fatalf("row holds %v, want 1000", row.Values[1])
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
	if got := db.Stats().Pages; got > 40 {
		t.Fatalf("1000 updates to one row grew the file to %d pages", got)
	}
}

func TestVersionsAreKeptWhileAReaderNeedsThem(t *testing.T) {
	db, _ := open(t)
	createPeople(t, db)
	insert(t, db, "people", value.Text("ada"), value.Integer(0))

	reader, err := db.Begin(false)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i := 1; i <= 20; i++ {
		if err := db.Update(func(tx *Tx) error {
			tbl, err := tx.Table("people")
			if err != nil {
				return err
			}
			_, err = tx.Update(tbl, 1, []value.Value{value.Text("ada"), value.Integer(int64(i))})
			return err
		}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	tbl, err := reader.Table("people")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	row, found, err := reader.Get(tbl, 1)
	if err != nil || !found {
		t.Fatalf("Get = (%v, %v, %v)", row, found, err)
	}
	if row.Values[1].Int() != 0 {
		t.Fatalf("the open snapshot sees %v, want the value it began with", row.Values[1])
	}
	if err := reader.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}
