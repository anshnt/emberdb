// Package bench compares emberdb against SQLite on the same workloads.
//
// It is a separate module so that the SQLite driver never becomes a dependency
// of emberdb itself. Run it with:
//
//	cd bench && go test -bench . -benchtime 3s
//
// Fairness notes, because a benchmark that quietly favours its author is worth
// nothing:
//
//   - Both engines are configured to fsync on every commit. SQLite runs in WAL
//     mode with synchronous=FULL, which is the closest match to what emberdb
//     does unconditionally.
//   - Neither side uses prepared statements. emberdb has none, so SQLite is
//     asked to parse each statement too. SQLite with prepared statements and
//     bound parameters is faster than the numbers here; this measures the two
//     engines doing the same work, not SQLite at its best.
//   - Bulk inserts are batched a thousand rows to a transaction on both sides,
//     so the measurement is the engine rather than the disk. CommitLatency
//     measures the other extreme, one row per transaction.
//   - Every benchmark starts from a fresh database file in a temporary
//     directory, and the setup is excluded from the timer.
package bench

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/anshnt/emberdb"
	_ "modernc.org/sqlite"
)

// rowsPerTransaction is how many rows a bulk load commits at once.
const rowsPerTransaction = 1000

// seedRows is how many rows the read benchmarks search through.
const seedRows = 100_000

// payload is the text stored beside each key, sized so that a row is
// representative rather than degenerate.
const payload = "the quick brown fox jumps over the lazy dog, again and again"

// openEmber creates a fresh emberdb database with the bench schema.
func openEmber(tb testing.TB, index bool) *emberdb.DB {
	tb.Helper()
	db, err := emberdb.Open(filepath.Join(tb.TempDir(), "bench.ember"))
	if err != nil {
		tb.Fatalf("emberdb.Open: %v", err)
	}
	tb.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE bench (k INTEGER NOT NULL, v TEXT NOT NULL)`); err != nil {
		tb.Fatalf("create table: %v", err)
	}
	if index {
		if _, err := db.Exec(`CREATE INDEX bench_k ON bench (k)`); err != nil {
			tb.Fatalf("create index: %v", err)
		}
	}
	return db
}

// openSQLite creates a fresh SQLite database with the same schema and the
// durability settings closest to emberdb's.
func openSQLite(tb testing.TB, index bool) *sql.DB {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "bench.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		tb.Fatalf("sql.Open: %v", err)
	}
	tb.Cleanup(func() { db.Close() })
	// One connection keeps the comparison honest: emberdb admits a single
	// writer, so letting SQLite pool connections would compare different
	// things.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			tb.Fatalf("%s: %v", pragma, err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE bench (k INTEGER NOT NULL, v TEXT NOT NULL)`); err != nil {
		tb.Fatalf("create table: %v", err)
	}
	if index {
		if _, err := db.Exec(`CREATE INDEX bench_k ON bench (k)`); err != nil {
			tb.Fatalf("create index: %v", err)
		}
	}
	return db
}

// keys returns n keys, either counting up or shuffled.
func keys(n int, shuffled bool, seed int64) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	if shuffled {
		r := rand.New(rand.NewSource(seed))
		r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	}
	return out
}

// insertEmber loads rows into emberdb, batching transactions.
func insertEmber(tb testing.TB, db *emberdb.DB, ks []int) {
	tb.Helper()
	for start := 0; start < len(ks); start += rowsPerTransaction {
		end := start + rowsPerTransaction
		if end > len(ks) {
			end = len(ks)
		}
		batch := ks[start:end]
		err := db.Update(func(tx *emberdb.Tx) error {
			for _, k := range batch {
				if _, err := tx.Exec(fmt.Sprintf(
					"INSERT INTO bench (k, v) VALUES (%d, '%s')", k, payload)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			tb.Fatalf("insert: %v", err)
		}
	}
}

// insertSQLite loads the same rows into SQLite, batched the same way.
func insertSQLite(tb testing.TB, db *sql.DB, ks []int) {
	tb.Helper()
	for start := 0; start < len(ks); start += rowsPerTransaction {
		end := start + rowsPerTransaction
		if end > len(ks) {
			end = len(ks)
		}
		tx, err := db.Begin()
		if err != nil {
			tb.Fatalf("begin: %v", err)
		}
		for _, k := range ks[start:end] {
			if _, err := tx.Exec(fmt.Sprintf(
				"INSERT INTO bench (k, v) VALUES (%d, '%s')", k, payload)); err != nil {
				tx.Rollback()
				tb.Fatalf("insert: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			tb.Fatalf("commit: %v", err)
		}
	}
}

func BenchmarkSequentialInsert(b *testing.B) {
	b.Run("emberdb", func(b *testing.B) {
		db := openEmber(b, false)
		ks := keys(b.N, false, 1)
		b.ResetTimer()
		insertEmber(b, db, ks)
	})
	b.Run("sqlite", func(b *testing.B) {
		db := openSQLite(b, false)
		ks := keys(b.N, false, 1)
		b.ResetTimer()
		insertSQLite(b, db, ks)
	})
}

func BenchmarkRandomInsert(b *testing.B) {
	b.Run("emberdb", func(b *testing.B) {
		db := openEmber(b, true)
		ks := keys(b.N, true, 2)
		b.ResetTimer()
		insertEmber(b, db, ks)
	})
	b.Run("sqlite", func(b *testing.B) {
		db := openSQLite(b, true)
		ks := keys(b.N, true, 2)
		b.ResetTimer()
		insertSQLite(b, db, ks)
	})
}

func BenchmarkPointRead(b *testing.B) {
	b.Run("emberdb", func(b *testing.B) {
		db := openEmber(b, true)
		insertEmber(b, db, keys(seedRows, false, 3))
		lookups := keys(b.N, true, 4)
		b.ResetTimer()
		for _, k := range lookups {
			result, err := db.Query(fmt.Sprintf("SELECT v FROM bench WHERE k = %d", k%seedRows))
			if err != nil {
				b.Fatalf("query: %v", err)
			}
			if len(result.Rows) != 1 {
				b.Fatalf("key %d returned %d rows", k%seedRows, len(result.Rows))
			}
		}
	})
	b.Run("sqlite", func(b *testing.B) {
		db := openSQLite(b, true)
		insertSQLite(b, db, keys(seedRows, false, 3))
		lookups := keys(b.N, true, 4)
		b.ResetTimer()
		for _, k := range lookups {
			var v string
			row := db.QueryRow(fmt.Sprintf("SELECT v FROM bench WHERE k = %d", k%seedRows))
			if err := row.Scan(&v); err != nil {
				b.Fatalf("query: %v", err)
			}
		}
	})
}

// rangeWidth is how many rows a range scan reads per operation.
const rangeWidth = 1000

func BenchmarkRangeScan(b *testing.B) {
	b.Run("emberdb", func(b *testing.B) {
		db := openEmber(b, true)
		insertEmber(b, db, keys(seedRows, false, 5))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			low := (i * 37) % (seedRows - rangeWidth)
			result, err := db.Query(fmt.Sprintf(
				"SELECT k, v FROM bench WHERE k >= %d AND k < %d", low, low+rangeWidth))
			if err != nil {
				b.Fatalf("query: %v", err)
			}
			if len(result.Rows) != rangeWidth {
				b.Fatalf("range from %d returned %d rows, want %d", low, len(result.Rows), rangeWidth)
			}
		}
	})
	b.Run("sqlite", func(b *testing.B) {
		db := openSQLite(b, true)
		insertSQLite(b, db, keys(seedRows, false, 5))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			low := (i * 37) % (seedRows - rangeWidth)
			rows, err := db.Query(fmt.Sprintf(
				"SELECT k, v FROM bench WHERE k >= %d AND k < %d", low, low+rangeWidth))
			if err != nil {
				b.Fatalf("query: %v", err)
			}
			count := 0
			for rows.Next() {
				var k int
				var v string
				if err := rows.Scan(&k, &v); err != nil {
					b.Fatalf("scan: %v", err)
				}
				count++
			}
			if err := rows.Err(); err != nil {
				b.Fatalf("rows: %v", err)
			}
			rows.Close()
			if count != rangeWidth {
				b.Fatalf("range from %d returned %d rows, want %d", low, count, rangeWidth)
			}
		}
	})
}

// BenchmarkCommitLatency is the opposite extreme from the bulk loads: one row
// per transaction, so the measurement is dominated by making each commit
// durable rather than by the engine.
func BenchmarkCommitLatency(b *testing.B) {
	b.Run("emberdb", func(b *testing.B) {
		db := openEmber(b, false)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := db.Exec(fmt.Sprintf("INSERT INTO bench (k, v) VALUES (%d, '%s')", i, payload)); err != nil {
				b.Fatalf("insert: %v", err)
			}
		}
	})
	b.Run("sqlite", func(b *testing.B) {
		db := openSQLite(b, false)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := db.Exec(fmt.Sprintf("INSERT INTO bench (k, v) VALUES (%d, '%s')", i, payload)); err != nil {
				b.Fatalf("insert: %v", err)
			}
		}
	})
}
