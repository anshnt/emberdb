package sql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/anshnt/emberdb/internal/value"
)

// parseOne parses a statement, failing the test if it does not.
func parseOne(t *testing.T, query string) Statement {
	t.Helper()
	statement, err := ParseOne(query)
	if err != nil {
		t.Fatalf("ParseOne(%q): %v", query, err)
	}
	return statement
}

func TestParseCreateTable(t *testing.T) {
	statement := parseOne(t, `CREATE TABLE notes (
		id INTEGER PRIMARY KEY,
		title TEXT NOT NULL UNIQUE,
		weight REAL,
		payload BLOB
	)`)
	create, ok := statement.(*CreateTable)
	if !ok {
		t.Fatalf("parsed %T, want *CreateTable", statement)
	}
	if create.Table != "notes" {
		t.Fatalf("table = %q", create.Table)
	}
	want := []ColumnDef{
		{Name: "id", Type: value.TypeInteger, PrimaryKey: true, NotNull: true},
		{Name: "title", Type: value.TypeText, NotNull: true, Unique: true},
		{Name: "weight", Type: value.TypeReal},
		{Name: "payload", Type: value.TypeBlob},
	}
	if len(create.Columns) != len(want) {
		t.Fatalf("parsed %d columns, want %d", len(create.Columns), len(want))
	}
	for i := range want {
		if create.Columns[i] != want[i] {
			t.Errorf("column %d = %+v, want %+v", i, create.Columns[i], want[i])
		}
	}
}

func TestParseCreateIndex(t *testing.T) {
	statement := parseOne(t, `CREATE UNIQUE INDEX notes_by_title ON notes (title)`)
	create, ok := statement.(*CreateIndex)
	if !ok {
		t.Fatalf("parsed %T, want *CreateIndex", statement)
	}
	if !create.Unique || create.Name != "notes_by_title" || create.Table != "notes" || create.Column != "title" {
		t.Fatalf("parsed %+v", create)
	}
	plain := parseOne(t, `CREATE INDEX i ON t (c)`).(*CreateIndex)
	if plain.Unique {
		t.Fatal("a plain CREATE INDEX came back unique")
	}
}

func TestParseInsertWithSeveralRows(t *testing.T) {
	statement := parseOne(t, `INSERT INTO notes (title, body) VALUES ('a', 'b'), ('c', NULL)`)
	insert, ok := statement.(*Insert)
	if !ok {
		t.Fatalf("parsed %T, want *Insert", statement)
	}
	if len(insert.Columns) != 2 || insert.Columns[0] != "title" {
		t.Fatalf("columns = %v", insert.Columns)
	}
	if len(insert.Rows) != 2 || len(insert.Rows[1]) != 2 {
		t.Fatalf("rows = %v", insert.Rows)
	}
	if literal, ok := insert.Rows[1][1].(*Literal); !ok || !literal.Value.IsNull() {
		t.Fatalf("second row's second value = %v", insert.Rows[1][1])
	}
}

func TestParseSelectClauses(t *testing.T) {
	statement := parseOne(t, `SELECT title AS t, weight * 2 FROM notes
		WHERE weight > 1.5 AND title IS NOT NULL
		ORDER BY weight DESC, title
		LIMIT 10 OFFSET 5`)
	sel, ok := statement.(*Select)
	if !ok {
		t.Fatalf("parsed %T, want *Select", statement)
	}
	if sel.Star {
		t.Fatal("Star should be false for an explicit column list")
	}
	if len(sel.Columns) != 2 || sel.Columns[0].Alias != "t" {
		t.Fatalf("columns = %+v", sel.Columns)
	}
	if sel.Where == nil {
		t.Fatal("WHERE was dropped")
	}
	if len(sel.OrderBy) != 2 || !sel.OrderBy[0].Descending || sel.OrderBy[1].Descending {
		t.Fatalf("ORDER BY = %+v", sel.OrderBy)
	}
	if sel.Limit == nil || sel.Offset == nil {
		t.Fatal("LIMIT or OFFSET was dropped")
	}
}

func TestParseStarSelect(t *testing.T) {
	sel := parseOne(t, `SELECT * FROM notes`).(*Select)
	if !sel.Star || len(sel.Columns) != 0 {
		t.Fatalf("parsed %+v", sel)
	}
}

func TestParseUpdateAndDelete(t *testing.T) {
	update := parseOne(t, `UPDATE notes SET title = 'x', weight = weight + 1 WHERE id = 3`).(*Update)
	if len(update.Assignments) != 2 || update.Assignments[1].Column != "weight" {
		t.Fatalf("assignments = %+v", update.Assignments)
	}
	if update.Where == nil {
		t.Fatal("WHERE was dropped")
	}
	del := parseOne(t, `DELETE FROM notes`).(*Delete)
	if del.Table != "notes" || del.Where != nil {
		t.Fatalf("parsed %+v", del)
	}
}

func TestParseTransactionControl(t *testing.T) {
	for _, query := range []string{"BEGIN", "BEGIN TRANSACTION", "COMMIT", "ROLLBACK TRANSACTION"} {
		if _, err := ParseOne(query); err != nil {
			t.Errorf("ParseOne(%q): %v", query, err)
		}
	}
}

func TestParseOperatorPrecedence(t *testing.T) {
	sel := parseOne(t, `SELECT 1 FROM t WHERE a = 1 + 2 * 3 OR b < 4 AND c > 5`).(*Select)
	// OR is loosest, so the root is OR.
	root, ok := sel.Where.(*Binary)
	if !ok || root.Op != "OR" {
		t.Fatalf("root of WHERE is %+v, want an OR", sel.Where)
	}
	left := root.Left.(*Binary)
	if left.Op != "=" {
		t.Fatalf("left of OR is %s, want the comparison", left.Op)
	}
	sum := left.Right.(*Binary)
	if sum.Op != "+" {
		t.Fatalf("right of = is %s, want +", sum.Op)
	}
	if product, ok := sum.Right.(*Binary); !ok || product.Op != "*" {
		t.Fatalf("* did not bind tighter than +: %+v", sum.Right)
	}
	right := root.Right.(*Binary)
	if right.Op != "AND" {
		t.Fatalf("right of OR is %s, want AND", right.Op)
	}
}

func TestParsePredicateForms(t *testing.T) {
	cases := map[string]any{
		`SELECT 1 FROM t WHERE a IS NULL`:             &IsNull{},
		`SELECT 1 FROM t WHERE a IS NOT NULL`:         &IsNull{},
		`SELECT 1 FROM t WHERE a BETWEEN 1 AND 2`:     &Between{},
		`SELECT 1 FROM t WHERE a NOT BETWEEN 1 AND 2`: &Between{},
		`SELECT 1 FROM t WHERE a IN (1, 2, 3)`:        &In{},
		`SELECT 1 FROM t WHERE a NOT IN (1)`:          &In{},
		`SELECT 1 FROM t WHERE a LIKE 'x%'`:           &Like{},
		`SELECT 1 FROM t WHERE a NOT LIKE 'x%'`:       &Like{},
	}
	for query, want := range cases {
		sel := parseOne(t, query).(*Select)
		if got := sel.Where; !sameType(got, want) {
			t.Errorf("%s parsed WHERE as %T, want %T", query, got, want)
		}
	}
	negated := parseOne(t, `SELECT 1 FROM t WHERE a NOT IN (1)`).(*Select).Where.(*In)
	if !negated.Negated {
		t.Error("NOT IN did not set Negated")
	}
	plain := parseOne(t, `SELECT 1 FROM t WHERE NOT a`).(*Select).Where
	if unary, ok := plain.(*Unary); !ok || unary.Op != "NOT" {
		t.Errorf("bare NOT parsed as %T", plain)
	}
}

func TestParseLiterals(t *testing.T) {
	sel := parseOne(t, `SELECT 42, -7, 1.5, 1e3, 'it''s', x'DEAD', NULL, TRUE, FALSE FROM t`).(*Select)
	want := []value.Value{
		value.Integer(42), value.Integer(-7), value.Real(1.5), value.Real(1000),
		value.Text("it's"), value.Blob([]byte{0xDE, 0xAD}), value.Null(),
		value.Integer(1), value.Integer(0),
	}
	if len(sel.Columns) != len(want) {
		t.Fatalf("parsed %d columns, want %d", len(sel.Columns), len(want))
	}
	for i, w := range want {
		literal, ok := sel.Columns[i].Expr.(*Literal)
		if !ok {
			t.Fatalf("column %d is %T, want a literal", i, sel.Columns[i].Expr)
		}
		if literal.Value.Kind() != w.Kind() || !value.Equal(literal.Value, w) {
			t.Errorf("column %d = %v (%s), want %v (%s)", i, literal.Value, literal.Value.Kind(), w, w.Kind())
		}
	}
}

func TestParseQuotedNamesAndComments(t *testing.T) {
	sel := parseOne(t, `-- leading comment
		SELECT "order" /* inline */ FROM "my table" -- trailing
	`).(*Select)
	if sel.Table != "my table" {
		t.Fatalf("table = %q", sel.Table)
	}
	ref, ok := sel.Columns[0].Expr.(*ColumnRef)
	if !ok || ref.Name != "order" {
		t.Fatalf("column = %+v", sel.Columns[0].Expr)
	}
}

func TestParseScript(t *testing.T) {
	statements, err := Parse(`
		CREATE TABLE t (a INTEGER);
		INSERT INTO t VALUES (1);
		SELECT * FROM t;
	`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(statements) != 3 {
		t.Fatalf("parsed %d statements, want 3", len(statements))
	}
	empty, err := Parse("   ;;  \n -- nothing here\n")
	if err != nil {
		t.Fatalf("Parse of an empty script: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("an empty script parsed to %d statements", len(empty))
	}
}

func TestParseErrorsAreUseful(t *testing.T) {
	cases := []struct {
		query string
		// want is a fragment the message must contain, chosen so the test
		// fails if the message stops saying what actually went wrong.
		want string
		line int
		col  int
	}{
		{`SELECT * notes`, `expected FROM after the select list`, 1, 10},
		{`SELECT * FROM`, `expected a table name, found end of input`, 1, 14},
		{`SELECT * FROM notes WHERE`, `expected a value, a column`, 1, 26},
		{`SELECT * FROM notes WHERE a =`, `expected a value, a column`, 1, 30},
		{`CREATE TABLE t (a BOOLEAN)`, `unknown column type BOOLEAN`, 1, 19},
		{`CREATE TABLE t (a INTEGER`, `expected ")" after the column list`, 1, 26},
		{`CREATE TABLE t`, `expected "(" after the table name`, 1, 15},
		{`CREATE TABLE order (a INTEGER)`, `reserved word ORDER`, 1, 14},
		{`INSERT INTO t VALUES 1`, `expected "(" before a row of values`, 1, 22},
		{`INSERT INTO t (a) SELECT`, `expected VALUES after the table name`, 1, 19},
		{`UPDATE t SET a`, `expected "=" after the column name`, 1, 15},
		{`DELETE t`, `expected FROM after DELETE`, 1, 8},
		{`SELECT 'unterminated FROM t`, `unterminated string literal`, 1, 8},
		{`SELECT x'GG' FROM t`, `even number of hexadecimal digits`, 1, 8},
		{`SELECT * FROM t WHERE a IS 5`, `expected NULL after IS`, 1, 28},
		{`SELECT * FROM t WHERE a IN 1`, `expected "(" after IN`, 1, 28},
		{`SELECT * FROM t WHERE a BETWEEN 1 2`, `expected AND between the bounds`, 1, 35},
		{`SELECT # FROM t`, `unexpected character '#'`, 1, 8},
		{`CREATE INDEX i ON t (a, b)`, `single column`, 1, 23},
		{`DROP TABLE t`, `DROP does not start a statement`, 1, 1},
		{`SELECT * FROM t SELECT`, `expected ";" or end of input`, 1, 17},
	}
	for _, c := range cases {
		_, err := ParseOne(c.query)
		if err == nil {
			t.Errorf("%s parsed without error", c.query)
			continue
		}
		parseErr, ok := err.(*Error)
		if !ok {
			t.Errorf("%s returned %T, want *Error", c.query, err)
			continue
		}
		if !strings.Contains(parseErr.Message, c.want) {
			t.Errorf("%s\n  message: %s\n  want it to contain: %s", c.query, parseErr.Message, c.want)
		}
		if parseErr.Line != c.line || parseErr.Column != c.col {
			t.Errorf("%s reported line %d column %d, want line %d column %d",
				c.query, parseErr.Line, parseErr.Column, c.line, c.col)
		}
	}
}

func TestParseErrorDetailPointsAtTheProblem(t *testing.T) {
	_, err := ParseOne("SELECT *\nFROM notes\nWHERE a = ")
	if err == nil {
		t.Fatal("expected an error")
	}
	detail := err.(*Error).Detail()
	lines := strings.Split(detail, "\n")
	if len(lines) != 3 {
		t.Fatalf("Detail returned %d lines:\n%s", len(lines), detail)
	}
	if !strings.Contains(lines[0], "line 3, column 11") {
		t.Errorf("first line does not carry the position: %s", lines[0])
	}
	if strings.TrimSpace(lines[1]) != "WHERE a =" {
		t.Errorf("second line should quote the source, got %q", lines[1])
	}
	caret := strings.Index(lines[2], "^")
	if caret != 12 {
		t.Errorf("caret is at %d in %q, want it under the end of the line", caret, lines[2])
	}
}

func TestErrorPositionsAdvanceAcrossLines(t *testing.T) {
	l := newLexer("a\n  bb\n\nccc")
	want := [][2]int{{1, 1}, {2, 3}, {4, 1}}
	for _, w := range want {
		tok, err := l.next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if tok.Line != w[0] || tok.Column != w[1] {
			t.Errorf("token %q at line %d column %d, want line %d column %d", tok.Lexeme, tok.Line, tok.Column, w[0], w[1])
		}
	}
}

// sameType reports whether two values have the same dynamic type.
func sameType(a, b any) bool {
	return a != nil && b != nil && reflect.TypeOf(a) == reflect.TypeOf(b)
}
