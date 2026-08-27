package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anshnt/emberdb"
)

// harness runs shell commands against a scratch database and captures output.
type harness struct {
	t     *testing.T
	db    *emberdb.DB
	shell *Shell
	out   *strings.Builder
	err   *strings.Builder
}

// newHarness opens a database and a shell reading from an empty file, so the
// shell is non-interactive and prints no prompts.
func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := emberdb.OpenWith(filepath.Join(dir, "test.ember"), emberdb.Options{NoSync: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { input.Close() })

	h := &harness{t: t, db: db, out: &strings.Builder{}, err: &strings.Builder{}}
	h.shell = NewShell(db, input, h.out, h.err, ModeTable, false)
	return h
}

// run executes a script and returns everything it printed.
func (h *harness) run(script string) string {
	h.t.Helper()
	h.out.Reset()
	h.err.Reset()
	if err := h.shell.RunScript(script); err != nil {
		h.shell.report(err)
	}
	return h.out.String()
}

// seed creates a table the output tests read from.
func (h *harness) seed() {
	h.t.Helper()
	h.run(`
		CREATE TABLE notes (id INTEGER PRIMARY KEY, title TEXT NOT NULL, words INTEGER);
		INSERT INTO notes (id, title, words) VALUES (1, 'pager', 820), (2, 'wal', 640);
	`)
}

func TestTableOutput(t *testing.T) {
	h := newHarness(t)
	h.seed()
	got := h.run(`SELECT title, words FROM notes ORDER BY id;`)
	want := strings.Join([]string{
		"┌───────┬───────┐",
		"│ title │ words │",
		"├───────┼───────┤",
		"│ pager │ 820   │",
		"│ wal   │ 640   │",
		"└───────┴───────┘",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("table output:\n%s\nwant:\n%s", got, want)
	}
}

func TestTableOutputWidensForWideValues(t *testing.T) {
	h := newHarness(t)
	h.run(`CREATE TABLE t (a TEXT); INSERT INTO t VALUES ('short'), ('a much longer value');`)
	got := h.run(`SELECT a FROM t ORDER BY a;`)
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if width := len([]rune(line)); width != 23 {
			t.Fatalf("line %q is %d runes wide, want every line the same width\n%s", line, width, got)
		}
	}
}

func TestCSVAndListOutput(t *testing.T) {
	h := newHarness(t)
	h.seed()
	h.run(`.mode csv`)
	if got := h.run(`SELECT title, words FROM notes ORDER BY id;`); got != "title,words\npager,820\nwal,640\n" {
		t.Fatalf("csv output:\n%q", got)
	}
	h.run(`.mode list`)
	if got := h.run(`SELECT title, words FROM notes ORDER BY id;`); got != "title|words\npager|820\nwal|640\n" {
		t.Fatalf("list output:\n%q", got)
	}
	h.run(`.mode table`)
	if !strings.Contains(h.run(`SELECT title FROM notes;`), "│") {
		t.Fatal("switching back to table mode did not take effect")
	}
	if err := h.shell.meta(".mode nonsense"); err == nil {
		t.Fatal(".mode accepted an unknown format")
	}
}

func TestCSVQuotesValuesThatNeedIt(t *testing.T) {
	h := newHarness(t)
	h.run(`CREATE TABLE t (a TEXT); INSERT INTO t VALUES ('has,comma'), ('has"quote');`)
	h.run(`.mode csv`)
	got := h.run(`SELECT a FROM t ORDER BY a;`)
	if !strings.Contains(got, `"has,comma"`) || !strings.Contains(got, `"has""quote"`) {
		t.Fatalf("csv did not quote correctly:\n%s", got)
	}
}

func TestDotTables(t *testing.T) {
	h := newHarness(t)
	if got := h.run(`.tables`); got != "(no tables)\n" {
		t.Fatalf(".tables on an empty database printed %q", got)
	}
	h.seed()
	h.run(`CREATE TABLE other (a INTEGER);`)
	if got := h.run(`.tables`); got != "notes  other\n" {
		t.Fatalf(".tables printed %q", got)
	}
}

func TestDotSchema(t *testing.T) {
	h := newHarness(t)
	h.seed()
	h.run(`CREATE INDEX notes_by_words ON notes (words);`)
	got := h.run(`.schema notes`)
	for _, fragment := range []string{
		"CREATE TABLE notes (",
		"id INTEGER PRIMARY KEY",
		"title TEXT NOT NULL",
		"CREATE INDEX notes_by_words ON notes (words);",
	} {
		if !strings.Contains(got, fragment) {
			t.Errorf(".schema is missing %q:\n%s", fragment, got)
		}
	}
	// Without an argument it covers every table.
	h.run(`CREATE TABLE other (a INTEGER);`)
	all := h.run(`.schema`)
	if !strings.Contains(all, "CREATE TABLE notes") || !strings.Contains(all, "CREATE TABLE other") {
		t.Errorf(".schema without an argument printed:\n%s", all)
	}
	if err := h.shell.meta(".schema missing"); err == nil {
		t.Error(".schema of a missing table should fail")
	}
}

func TestDotTimer(t *testing.T) {
	h := newHarness(t)
	h.seed()
	if got := h.run(`SELECT title FROM notes;`); strings.Contains(got, "time:") {
		t.Fatalf("the timer is on before being asked for:\n%s", got)
	}
	h.run(`.timer on`)
	got := h.run(`SELECT title FROM notes;`)
	if !strings.Contains(got, "time:") {
		t.Fatalf(".timer on did not report a time:\n%s", got)
	}
	if !strings.Contains(got, "plan: scan notes") {
		t.Fatalf(".timer on did not report the plan:\n%s", got)
	}
	h.run(`CREATE INDEX notes_by_words ON notes (words);`)
	indexed := h.run(`SELECT title FROM notes WHERE words = 640;`)
	if !strings.Contains(indexed, "using index notes_by_words") {
		t.Fatalf("the plan does not mention the index:\n%s", indexed)
	}
	h.run(`.timer off`)
	if got := h.run(`SELECT title FROM notes;`); strings.Contains(got, "time:") {
		t.Fatalf(".timer off left it on:\n%s", got)
	}
	if err := h.shell.meta(".timer maybe"); err == nil {
		t.Error(".timer accepted an argument that is not on or off")
	}
	if err := h.shell.meta(".timer"); err == nil {
		t.Error(".timer with no argument should fail")
	}
}

func TestDotRead(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "seed.sql")
	contents := `-- a comment
		CREATE TABLE fromfile (a INTEGER, b TEXT);
		INSERT INTO fromfile VALUES (1, 'one');
		.mode list
		SELECT b FROM fromfile;
	`
	if err := os.WriteFile(script, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := h.run(".read " + script)
	if !strings.Contains(got, "b\none\n") {
		t.Fatalf(".read output:\n%s", got)
	}
	if err := h.shell.meta(".read " + filepath.Join(dir, "missing.sql")); err == nil {
		t.Error(".read of a missing file should fail")
	}
}

func TestDotReadRefusesToRecurseForever(t *testing.T) {
	h := newHarness(t)
	script := filepath.Join(t.TempDir(), "loop.sql")
	if err := os.WriteFile(script, []byte(".read "+script+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := h.shell.RunFile(script)
	if err == nil {
		t.Fatal("a script that reads itself should stop with an error")
	}
	if !strings.Contains(err.Error(), "nested too deeply") {
		t.Fatalf("error = %v", err)
	}
}

func TestDotStats(t *testing.T) {
	h := newHarness(t)
	h.seed()
	got := h.run(`.stats`)
	for _, fragment := range []string{"file", "pages", "cache", "log", "last commit"} {
		if !strings.Contains(got, fragment) {
			t.Errorf(".stats is missing %q:\n%s", fragment, got)
		}
	}
}

func TestUnknownDotCommand(t *testing.T) {
	h := newHarness(t)
	err := h.shell.meta(".nope")
	if err == nil || !strings.Contains(err.Error(), "try .help") {
		t.Fatalf("unknown command error = %v", err)
	}
	if got := h.run(`.help`); !strings.Contains(got, ".schema") {
		t.Fatalf(".help output:\n%s", got)
	}
}

func TestDotQuitStopsAScript(t *testing.T) {
	h := newHarness(t)
	h.run(`
		CREATE TABLE t (a INTEGER);
		.quit
		INSERT INTO t VALUES (1);
	`)
	h.shell.quit = false
	if got := h.run(`SELECT a FROM t;`); strings.Contains(got, "1") {
		t.Fatalf("statements after .quit still ran:\n%s", got)
	}
}

func TestSyntaxErrorsArePrintedWithAPointer(t *testing.T) {
	h := newHarness(t)
	h.run(`SELECT * FRM notes;`)
	got := h.err.String()
	if !strings.Contains(got, "syntax error at line 1, column 10") {
		t.Fatalf("error output:\n%s", got)
	}
	if !strings.Contains(got, "^") {
		t.Fatalf("error output has no caret:\n%s", got)
	}
}

func TestMultiLineStatements(t *testing.T) {
	h := newHarness(t)
	got := h.run("CREATE TABLE t (\n  a INTEGER,\n  b TEXT\n);\nINSERT INTO t VALUES (1, 'x');\n.mode list\nSELECT b FROM t;\n")
	if !strings.Contains(got, "b\nx\n") {
		t.Fatalf("a statement spread over lines produced:\n%s", got)
	}
}

func TestStatementWithoutASemicolonAtEndOfInput(t *testing.T) {
	h := newHarness(t)
	h.run(`CREATE TABLE t (a INTEGER); INSERT INTO t VALUES (7);`)
	h.run(`.mode list`)
	if got := h.run("SELECT a FROM t"); !strings.Contains(got, "a\n7\n") {
		t.Fatalf("a final statement without a semicolon produced:\n%s", got)
	}
}

func TestParseModes(t *testing.T) {
	for name, want := range map[string]Mode{
		"table": ModeTable, "TABLE": ModeTable, "csv": ModeCSV, "list": ModeList,
	} {
		got, ok := ParseMode(name)
		if !ok || got != want {
			t.Errorf("ParseMode(%q) = (%v, %v)", name, got, ok)
		}
	}
	if _, ok := ParseMode("json"); ok {
		t.Error("ParseMode accepted an unsupported mode")
	}
}

func TestPlural(t *testing.T) {
	if plural(1) != "" || plural(0) != "s" || plural(2) != "s" {
		t.Error("plural is wrong")
	}
}
