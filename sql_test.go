package emberdb_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anshnt/emberdb"
)

// open creates a database on a temporary file.
func open(t *testing.T) *emberdb.DB {
	t.Helper()
	db, err := emberdb.OpenWith(filepath.Join(t.TempDir(), "test.ember"), emberdb.Options{NoSync: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// mustExec runs a script and fails the test on error.
func mustExec(t *testing.T, db *emberdb.DB, query string) *emberdb.Result {
	t.Helper()
	result, err := db.Exec(query)
	if err != nil {
		t.Fatalf("Exec(%q): %v", truncate(query), err)
	}
	return result
}

// truncate shortens a query for an error message.
func truncate(query string) string {
	query = strings.Join(strings.Fields(query), " ")
	if len(query) > 70 {
		return query[:70] + "..."
	}
	return query
}

// rowsOf renders a result's rows as comma-joined strings, for comparison.
func rowsOf(result *emberdb.Result) []string {
	out := make([]string, len(result.Rows))
	for i, row := range result.Rows {
		parts := make([]string, len(row))
		for j, v := range row {
			parts[j] = v.String()
		}
		out[i] = strings.Join(parts, ",")
	}
	return out
}

// wantRows checks a query's rows against the expected rendering.
func wantRows(t *testing.T, db *emberdb.DB, query string, want ...string) {
	t.Helper()
	result, err := db.Query(query)
	if err != nil {
		t.Fatalf("Query(%q): %v", truncate(query), err)
	}
	got := rowsOf(result)
	if len(got) != len(want) {
		t.Fatalf("%s\n  returned %v\n  want     %v", truncate(query), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s\n  row %d is %q, want %q\n  all: %v", truncate(query), i, got[i], want[i], got)
		}
	}
}

// seedPeople creates and fills a table used by several tests.
func seedPeople(t *testing.T, db *emberdb.DB) {
	t.Helper()
	mustExec(t, db, `CREATE TABLE people (name TEXT NOT NULL, age INTEGER, height REAL)`)
	mustExec(t, db, `INSERT INTO people (name, age, height) VALUES
		('ada', 36, 1.7),
		('grace', 45, 1.6),
		('alan', 41, 1.8),
		('edsger', NULL, 1.75)`)
}

func TestEndToEndSelect(t *testing.T) {
	db := open(t)
	seedPeople(t, db)

	wantRows(t, db, `SELECT name FROM people WHERE age > 40 ORDER BY name`, "alan", "grace")
	wantRows(t, db, `SELECT name, age FROM people ORDER BY age DESC LIMIT 2`, "grace,45", "alan,41")
	wantRows(t, db, `SELECT name FROM people ORDER BY name LIMIT 2 OFFSET 1`, "alan", "edsger")
	wantRows(t, db, `SELECT name FROM people WHERE age IS NULL`, "edsger")
	wantRows(t, db, `SELECT name FROM people WHERE age IS NOT NULL ORDER BY name`, "ada", "alan", "grace")
	wantRows(t, db, `SELECT name FROM people WHERE age BETWEEN 36 AND 41 ORDER BY name`, "ada", "alan")
	wantRows(t, db, `SELECT name FROM people WHERE name IN ('ada', 'nobody') ORDER BY name`, "ada")
	wantRows(t, db, `SELECT name FROM people WHERE name LIKE 'a%' ORDER BY name`, "ada", "alan")
	wantRows(t, db, `SELECT name FROM people WHERE name LIKE '_da'`, "ada")
	wantRows(t, db, `SELECT name || ' is ' || age FROM people WHERE name = 'ada'`, "ada is 36")
	wantRows(t, db, `SELECT age * 2 FROM people WHERE name = 'ada'`, "72")
	wantRows(t, db, `SELECT height + 1 FROM people WHERE name = 'grace'`, "2.6")
}

func TestSelectColumnNames(t *testing.T) {
	db := open(t)
	seedPeople(t, db)
	result, err := db.Query(`SELECT * FROM people LIMIT 1`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	want := []string{"name", "age", "height"}
	if len(result.Columns) != len(want) {
		t.Fatalf("columns = %v, want %v", result.Columns, want)
	}
	for i := range want {
		if result.Columns[i] != want[i] {
			t.Fatalf("column %d = %q, want %q", i, result.Columns[i], want[i])
		}
	}
	aliased, err := db.Query(`SELECT age AS years, age * 2 FROM people LIMIT 1`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if aliased.Columns[0] != "years" || aliased.Columns[1] != "?column?" {
		t.Fatalf("columns = %v", aliased.Columns)
	}
}

func TestNullSemantics(t *testing.T) {
	db := open(t)
	seedPeople(t, db)

	// A comparison against null is unknown, and unknown rows are excluded.
	wantRows(t, db, `SELECT name FROM people WHERE age = NULL`)
	wantRows(t, db, `SELECT name FROM people WHERE age != NULL`)
	wantRows(t, db, `SELECT name FROM people WHERE NOT (age = NULL)`)
	// Null is not true, so an OR with a true side still passes.
	wantRows(t, db, `SELECT name FROM people WHERE age = NULL OR name = 'ada'`, "ada")
	// An AND with a false side is false even with a null side.
	wantRows(t, db, `SELECT name FROM people WHERE age = NULL AND 1 = 0`)
	// Arithmetic on null is null.
	wantRows(t, db, `SELECT age + 1 FROM people WHERE name = 'edsger'`, "NULL")
	// IN against a list containing null: a hit is still a hit.
	wantRows(t, db, `SELECT name FROM people WHERE age IN (36, NULL)`, "ada")
	wantRows(t, db, `SELECT name FROM people WHERE age IN (99, NULL)`)
	// ORDER BY puts nulls first.
	wantRows(t, db, `SELECT name FROM people ORDER BY age`, "edsger", "ada", "alan", "grace")
	wantRows(t, db, `SELECT name FROM people ORDER BY age DESC`, "grace", "alan", "ada", "edsger")
}

func TestUpdateAndDelete(t *testing.T) {
	db := open(t)
	seedPeople(t, db)

	result := mustExec(t, db, `UPDATE people SET age = age + 1 WHERE age IS NOT NULL`)
	if result.RowsAffected != 3 {
		t.Fatalf("UPDATE affected %d rows, want 3", result.RowsAffected)
	}
	wantRows(t, db, `SELECT name, age FROM people ORDER BY name`, "ada,37", "alan,42", "edsger,NULL", "grace,46")

	// Assignments see the row as it was, so a swap really swaps.
	mustExec(t, db, `CREATE TABLE pairs (a INTEGER, b INTEGER)`)
	mustExec(t, db, `INSERT INTO pairs VALUES (1, 2)`)
	mustExec(t, db, `UPDATE pairs SET a = b, b = a`)
	wantRows(t, db, `SELECT a, b FROM pairs`, "2,1")

	deleted := mustExec(t, db, `DELETE FROM people WHERE age > 41`)
	if deleted.RowsAffected != 2 {
		t.Fatalf("DELETE affected %d rows, want 2", deleted.RowsAffected)
	}
	wantRows(t, db, `SELECT name FROM people ORDER BY name`, "ada", "edsger")

	all := mustExec(t, db, `DELETE FROM people`)
	if all.RowsAffected != 2 {
		t.Fatalf("DELETE without WHERE affected %d rows, want 2", all.RowsAffected)
	}
	wantRows(t, db, `SELECT name FROM people`)
}

func TestInsertVariants(t *testing.T) {
	db := open(t)
	mustExec(t, db, `CREATE TABLE t (a INTEGER, b TEXT, c REAL)`)
	// Positional, without a column list.
	mustExec(t, db, `INSERT INTO t VALUES (1, 'one', 1.5)`)
	// Named, in a different order, leaving a column out.
	result := mustExec(t, db, `INSERT INTO t (c, a) VALUES (2.5, 2)`)
	if result.LastInsertID != 2 {
		t.Fatalf("LastInsertID = %d, want 2", result.LastInsertID)
	}
	wantRows(t, db, `SELECT a, b, c FROM t ORDER BY a`, "1,one,1.5", "2,NULL,2.5")

	// Several rows at once.
	many := mustExec(t, db, `INSERT INTO t (a) VALUES (3), (4), (5)`)
	if many.RowsAffected != 3 || many.LastInsertID != 5 {
		t.Fatalf("multi-row insert = %+v", many)
	}
}

func TestInsertErrors(t *testing.T) {
	db := open(t)
	mustExec(t, db, `CREATE TABLE t (a INTEGER NOT NULL, b TEXT)`)
	cases := []struct {
		query string
		want  error
	}{
		{`INSERT INTO missing VALUES (1)`, emberdb.ErrNoSuchTable},
		{`INSERT INTO t (nope) VALUES (1)`, emberdb.ErrNoSuchColumn},
		{`INSERT INTO t (a) VALUES (NULL)`, emberdb.ErrConstraint},
		{`INSERT INTO t (a) VALUES ('text')`, emberdb.ErrTypeMismatch},
	}
	for _, c := range cases {
		if _, err := db.Exec(c.query); !errors.Is(err, c.want) {
			t.Errorf("%s\n  returned %v\n  want     %v", c.query, err, c.want)
		}
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (1)`); err == nil {
		t.Error("an INSERT with too few values succeeded")
	}
	if _, err := db.Exec(`INSERT INTO t (a, a) VALUES (1, 2)`); err == nil {
		t.Error("an INSERT assigning a column twice succeeded")
	}
}

func TestIndexesAreUsedAndCorrect(t *testing.T) {
	db := open(t)
	mustExec(t, db, `CREATE TABLE events (kind TEXT, at INTEGER)`)
	var b strings.Builder
	b.WriteString(`INSERT INTO events (kind, at) VALUES `)
	for i := 0; i < 2000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "('k%d', %d)", i%7, i)
	}
	mustExec(t, db, b.String())

	before, err := db.Query(`SELECT kind FROM events WHERE at = 1234`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !strings.HasPrefix(before.Plan, "scan ") {
		t.Fatalf("without an index the plan is %q, want a full scan", before.Plan)
	}

	mustExec(t, db, `CREATE INDEX events_at ON events (at)`)
	after, err := db.Query(`SELECT kind FROM events WHERE at = 1234`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !strings.Contains(after.Plan, "using index events_at") {
		t.Fatalf("with an index the plan is %q, want an index search", after.Plan)
	}
	if len(after.Rows) != 1 || after.Rows[0][0].Str() != before.Rows[0][0].Str() {
		t.Fatalf("the index returned %v, the scan returned %v", rowsOf(after), rowsOf(before))
	}

	// A range through the index must agree with the same range without it.
	ranged, err := db.Query(`SELECT at FROM events WHERE at >= 100 AND at < 110 ORDER BY at`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(ranged.Rows) != 10 {
		t.Fatalf("range query returned %d rows, want 10: %v", len(ranged.Rows), rowsOf(ranged))
	}
	if ranged.Rows[0][0].Int() != 100 || ranged.Rows[9][0].Int() != 109 {
		t.Fatalf("range query returned %v", rowsOf(ranged))
	}

	// The index has to keep up with writes.
	mustExec(t, db, `UPDATE events SET at = -1 WHERE at = 105`)
	wantRows(t, db, `SELECT at FROM events WHERE at = 105`)
	wantRows(t, db, `SELECT kind FROM events WHERE at = -1`, "k0")
	mustExec(t, db, `DELETE FROM events WHERE at = -1`)
	wantRows(t, db, `SELECT kind FROM events WHERE at = -1`)
}

func TestIndexIsSkippedWhenItCannotAnswer(t *testing.T) {
	db := open(t)
	mustExec(t, db, `CREATE TABLE t (a INTEGER, b TEXT)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'x'), (2, 'y')`)
	mustExec(t, db, `CREATE INDEX t_a ON t (a)`)

	// A bound of the wrong type cannot be compared as index bytes, so the
	// planner has to fall back rather than return the wrong rows.
	result, err := db.Query(`SELECT b FROM t WHERE a = 'x'`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if strings.Contains(result.Plan, "index") {
		t.Fatalf("plan %q used the index for a mistyped bound", result.Plan)
	}
	if len(result.Rows) != 0 {
		t.Fatalf("comparing an integer column to text matched %v", rowsOf(result))
	}
	// An OR cannot be reduced to one range either.
	or, err := db.Query(`SELECT b FROM t WHERE a = 1 OR b = 'y'`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if strings.Contains(or.Plan, "index") {
		t.Fatalf("plan %q used the index for a disjunction", or.Plan)
	}
	if len(or.Rows) != 2 {
		t.Fatalf("OR returned %v", rowsOf(or))
	}
}

func TestTransactionsThroughSQL(t *testing.T) {
	db := open(t)
	seedPeople(t, db)

	mustExec(t, db, `BEGIN`)
	if !db.InTransaction() {
		t.Fatal("InTransaction is false after BEGIN")
	}
	mustExec(t, db, `INSERT INTO people (name, age, height) VALUES ('temp', 1, 1.0)`)
	wantRows(t, db, `SELECT name FROM people WHERE name = 'temp'`, "temp")
	mustExec(t, db, `ROLLBACK`)
	if db.InTransaction() {
		t.Fatal("InTransaction is true after ROLLBACK")
	}
	wantRows(t, db, `SELECT name FROM people WHERE name = 'temp'`)

	mustExec(t, db, `BEGIN`)
	mustExec(t, db, `INSERT INTO people (name, age, height) VALUES ('kept', 2, 1.0)`)
	mustExec(t, db, `COMMIT`)
	wantRows(t, db, `SELECT name FROM people WHERE name = 'kept'`, "kept")

	if _, err := db.Exec(`COMMIT`); !errors.Is(err, emberdb.ErrNoTransaction) {
		t.Fatalf("COMMIT with nothing open = %v, want ErrNoTransaction", err)
	}
	mustExec(t, db, `BEGIN`)
	if _, err := db.Exec(`BEGIN`); !errors.Is(err, emberdb.ErrTransactionOpen) {
		t.Fatalf("nested BEGIN = %v, want ErrTransactionOpen", err)
	}
	mustExec(t, db, `ROLLBACK`)
}

func TestProgrammaticTransactions(t *testing.T) {
	db := open(t)
	seedPeople(t, db)

	sentinel := errors.New("give up")
	err := db.Update(func(tx *emberdb.Tx) error {
		if _, err := tx.Exec(`INSERT INTO people (name, age, height) VALUES ('ghost', 1, 1.0)`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Update = %v, want the callback's error", err)
	}
	wantRows(t, db, `SELECT name FROM people WHERE name = 'ghost'`)

	if err := db.Update(func(tx *emberdb.Tx) error {
		_, err := tx.Exec(`INSERT INTO people (name, age, height) VALUES ('real', 1, 1.0)`)
		return err
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	wantRows(t, db, `SELECT name FROM people WHERE name = 'real'`, "real")

	if err := db.View(func(tx *emberdb.Tx) error {
		result, err := tx.Exec(`SELECT name FROM people WHERE name = 'real'`)
		if err != nil {
			return err
		}
		if len(result.Rows) != 1 {
			t.Fatalf("View saw %v", rowsOf(result))
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestSchemaIntrospection(t *testing.T) {
	db := open(t)
	mustExec(t, db, `CREATE TABLE notes (id INTEGER PRIMARY KEY, title TEXT NOT NULL, body TEXT)`)
	mustExec(t, db, `CREATE INDEX notes_by_title ON notes (title)`)
	mustExec(t, db, `INSERT INTO notes (id, title) VALUES (1, 'a'), (2, 'b')`)

	names, err := db.Tables()
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	if len(names) != 1 || names[0] != "notes" {
		t.Fatalf("Tables = %v", names)
	}
	info, err := db.TableInfo("NOTES")
	if err != nil {
		t.Fatalf("TableInfo: %v", err)
	}
	if info.Rows != 2 || len(info.Columns) != 3 {
		t.Fatalf("TableInfo = %+v", info)
	}
	if !info.Columns[0].PrimaryKey || !info.Columns[1].NotNull {
		t.Fatalf("column flags = %+v", info.Columns)
	}
	ddl := info.DDL()
	for _, fragment := range []string{
		"CREATE TABLE notes (",
		"id INTEGER PRIMARY KEY",
		"title TEXT NOT NULL",
		"CREATE INDEX notes_by_title ON notes (title);",
	} {
		if !strings.Contains(ddl, fragment) {
			t.Errorf("DDL is missing %q:\n%s", fragment, ddl)
		}
	}
	if strings.Contains(ddl, "emberdb_auto_") {
		t.Errorf("DDL exposes the implicit primary-key index:\n%s", ddl)
	}
}

func TestIfNotExists(t *testing.T) {
	db := open(t)
	mustExec(t, db, `CREATE TABLE t (a INTEGER)`)
	if _, err := db.Exec(`CREATE TABLE t (a INTEGER)`); !errors.Is(err, emberdb.ErrTableExists) {
		t.Fatalf("duplicate CREATE TABLE = %v", err)
	}
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS t (a INTEGER)`)
	mustExec(t, db, `CREATE INDEX t_a ON t (a)`)
	if _, err := db.Exec(`CREATE INDEX t_a ON t (a)`); !errors.Is(err, emberdb.ErrIndexExists) {
		t.Fatalf("duplicate CREATE INDEX = %v", err)
	}
	mustExec(t, db, `CREATE INDEX IF NOT EXISTS t_a ON t (a)`)
}

func TestEvaluationErrors(t *testing.T) {
	db := open(t)
	seedPeople(t, db)
	cases := []string{
		`SELECT nope FROM people`,
		`SELECT * FROM people WHERE nope = 1`,
		`SELECT 1 / 0 FROM people`,
		`SELECT name + 1 FROM people`,
		`UPDATE people SET nope = 1`,
		`SELECT * FROM people LIMIT -1`,
		`SELECT * FROM people LIMIT 'x'`,
	}
	for _, query := range cases {
		if _, err := db.Exec(query); err == nil {
			t.Errorf("%s succeeded, want an error", query)
		}
	}
	if _, err := db.Exec(`SELECT * FROM people WHERE nope = 1`); !errors.Is(err, emberdb.ErrNoSuchColumn) {
		t.Error("an unknown column in WHERE should report ErrNoSuchColumn")
	}
}

func TestSyntaxErrorsSurfaceThroughTheAPI(t *testing.T) {
	db := open(t)
	_, err := db.Exec(`SELECT * FRM t`)
	var syntax *emberdb.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("Exec returned %T (%v), want a *SyntaxError", err, err)
	}
	if syntax.Line != 1 || syntax.Column != 10 {
		t.Fatalf("error is at line %d column %d", syntax.Line, syntax.Column)
	}
}

func TestScriptStopsAtTheFirstError(t *testing.T) {
	db := open(t)
	_, err := db.ExecAll(`
		CREATE TABLE t (a INTEGER);
		INSERT INTO t VALUES (1);
		INSERT INTO t VALUES ('bad');
		INSERT INTO t VALUES (2);
	`)
	if err == nil {
		t.Fatal("the script should have failed")
	}
	// Statements before the failure are already committed, since each runs
	// in a transaction of its own.
	wantRows(t, db, `SELECT a FROM t`, "1")
}

func TestBlobRoundTrip(t *testing.T) {
	db := open(t)
	mustExec(t, db, `CREATE TABLE files (name TEXT, data BLOB)`)
	mustExec(t, db, `INSERT INTO files VALUES ('empty', x''), ('small', x'00FF10')`)
	wantRows(t, db, `SELECT name, data FROM files ORDER BY name`, "empty,x''", "small,x'00ff10'")

	// A blob larger than a page has to survive the overflow path.
	big := strings.Repeat("ab", 40000)
	mustExec(t, db, fmt.Sprintf(`INSERT INTO files VALUES ('big', x'%s')`, big))
	result, err := db.Query(`SELECT data FROM files WHERE name = 'big'`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := len(result.Rows[0][0].Bytes()); got != len(big)/2 {
		t.Fatalf("blob came back as %d bytes, want %d", got, len(big)/2)
	}
}

func TestDurabilityAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.ember")
	db, err := emberdb.OpenWith(path, emberdb.Options{NoSync: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustExec(t, db, `CREATE TABLE t (a INTEGER, b TEXT)`)
	for i := 0; i < 500; i++ {
		mustExec(t, db, fmt.Sprintf(`INSERT INTO t VALUES (%d, 'row%d')`, i, i))
	}
	mustExec(t, db, `CREATE INDEX t_a ON t (a)`)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := emberdb.OpenWith(path, emberdb.Options{NoSync: true})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	wantRows(t, reopened, `SELECT b FROM t WHERE a = 250`, "row250")
	result, err := reopened.Query(`SELECT a FROM t WHERE a >= 100 AND a < 105 ORDER BY a`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Rows) != 5 || !strings.Contains(result.Plan, "index") {
		t.Fatalf("after reopen the index query returned %d rows with plan %q", len(result.Rows), result.Plan)
	}
}
