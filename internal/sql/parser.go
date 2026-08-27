package sql

import (
	"strings"

	"github.com/anshnt/emberdb/internal/value"
)

// Parse parses a script of semicolon-separated statements.
func Parse(query string) ([]Statement, error) {
	p, err := newParser(query)
	if err != nil {
		return nil, err
	}
	var statements []Statement
	for {
		for p.acceptSymbol(";") {
		}
		if p.at().Kind == KindEOF {
			return statements, nil
		}
		statement, err := p.statement()
		if err != nil {
			return nil, err
		}
		statements = append(statements, statement)
		if p.at().Kind == KindEOF {
			return statements, nil
		}
		if !p.acceptSymbol(";") {
			return nil, p.errorHere("expected %q or end of input after a statement, found %s", ";", p.at().Describe())
		}
	}
}

// ParseOne parses a query that must hold exactly one statement.
func ParseOne(query string) (Statement, error) {
	statements, err := Parse(query)
	if err != nil {
		return nil, err
	}
	switch len(statements) {
	case 0:
		return nil, &Error{Message: "empty statement", Line: 1, Column: 1, Query: query}
	case 1:
		return statements[0], nil
	default:
		return nil, &Error{Message: "expected a single statement", Line: 1, Column: 1, Query: query}
	}
}

// parser is a recursive-descent parser over a pre-lexed token stream. Lexing
// everything up front costs nothing at these sizes and makes lookahead, which
// the grammar needs in a few places, trivial.
type parser struct {
	query  string
	tokens []Token
	pos    int
}

// newParser lexes the whole query.
func newParser(query string) (*parser, error) {
	p := &parser{query: query}
	l := newLexer(query)
	for {
		tok, err := l.next()
		if err != nil {
			return nil, err
		}
		p.tokens = append(p.tokens, tok)
		if tok.Kind == KindEOF {
			return p, nil
		}
	}
}

// at returns the current token.
func (p *parser) at() Token { return p.tokens[p.pos] }

// peek returns the token n positions ahead, clamped to the end marker.
func (p *parser) peek(n int) Token {
	if p.pos+n >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1]
	}
	return p.tokens[p.pos+n]
}

// advance consumes the current token and returns it.
func (p *parser) advance() Token {
	tok := p.tokens[p.pos]
	if p.pos < len(p.tokens)-1 {
		p.pos++
	}
	return tok
}

// acceptKeyword consumes a keyword if it is next.
func (p *parser) acceptKeyword(word string) bool {
	if p.at().is(KindKeyword, word) {
		p.advance()
		return true
	}
	return false
}

// acceptNull consumes a NULL keyword. NULL lexes as a literal rather than a
// keyword, because that is what it is everywhere except in a constraint, so
// the places that want it as a word ask for it here.
func (p *parser) acceptNull() bool {
	if p.at().Kind == KindLiteral && p.at().Value.IsNull() {
		p.advance()
		return true
	}
	return false
}

// acceptSymbol consumes a symbol if it is next.
func (p *parser) acceptSymbol(sym string) bool {
	if p.at().is(KindSymbol, sym) {
		p.advance()
		return true
	}
	return false
}

// errorHere builds an error positioned at the current token.
func (p *parser) errorHere(format string, args ...any) *Error {
	return errorAt(p.query, p.at(), format, args...)
}

// expectKeyword consumes a keyword or reports what was found instead. context
// explains where the keyword was expected, for example "after DELETE".
func (p *parser) expectKeyword(word, context string) *Error {
	if p.acceptKeyword(word) {
		return nil
	}
	return p.errorHere("expected %s %s, found %s", word, context, p.at().Describe())
}

// expectSymbol consumes a symbol or reports what was found instead.
func (p *parser) expectSymbol(sym, context string) *Error {
	if p.acceptSymbol(sym) {
		return nil
	}
	return p.errorHere("expected %q %s, found %s", sym, context, p.at().Describe())
}

// expectName consumes an identifier. Keywords are rejected with a message that
// says so, since "CREATE TABLE order" is a mistake worth naming.
func (p *parser) expectName(what string) (string, *Error) {
	tok := p.at()
	if tok.Kind == KindIdent {
		p.advance()
		return tok.Lexeme, nil
	}
	if tok.Kind == KindKeyword {
		return "", p.errorHere("expected %s, found the reserved word %s; quote it as %q to use it as a name", what, tok.Lexeme, strings.ToLower(tok.Lexeme))
	}
	return "", p.errorHere("expected %s, found %s", what, tok.Describe())
}

// statement dispatches on the leading keyword.
func (p *parser) statement() (Statement, *Error) {
	tok := p.at()
	if tok.Kind != KindKeyword {
		return nil, p.errorHere("expected a statement, found %s", tok.Describe())
	}
	switch tok.Lexeme {
	case "CREATE":
		return p.createStatement()
	case "INSERT":
		return p.insertStatement()
	case "SELECT":
		return p.selectStatement()
	case "UPDATE":
		return p.updateStatement()
	case "DELETE":
		return p.deleteStatement()
	case "BEGIN":
		p.advance()
		p.acceptKeyword("TRANSACTION")
		return &Begin{}, nil
	case "COMMIT":
		p.advance()
		p.acceptKeyword("TRANSACTION")
		return &Commit{}, nil
	case "ROLLBACK":
		p.advance()
		p.acceptKeyword("TRANSACTION")
		return &Rollback{}, nil
	case "DROP":
		return nil, p.errorHere("DROP does not start a statement emberdb understands; it has no DROP TABLE or DROP INDEX")
	default:
		return nil, p.errorHere("%s does not start a statement emberdb understands", tok.Lexeme)
	}
}

// ifNotExists consumes an optional IF NOT EXISTS clause.
func (p *parser) ifNotExists() (bool, *Error) {
	if !p.acceptKeyword("IF") {
		return false, nil
	}
	if err := p.expectKeyword("NOT", "after IF"); err != nil {
		return false, err
	}
	if err := p.expectKeyword("EXISTS", "after IF NOT"); err != nil {
		return false, err
	}
	return true, nil
}

// createStatement parses CREATE TABLE and CREATE INDEX.
func (p *parser) createStatement() (Statement, *Error) {
	p.advance() // CREATE
	unique := p.acceptKeyword("UNIQUE")
	switch {
	case unique && p.at().is(KindKeyword, "INDEX"):
		return p.createIndex(true)
	case p.acceptKeyword("TABLE"):
		return p.createTable()
	case p.at().is(KindKeyword, "INDEX"):
		return p.createIndex(false)
	case unique:
		return nil, p.errorHere("expected INDEX after CREATE UNIQUE, found %s", p.at().Describe())
	default:
		return nil, p.errorHere("expected TABLE or INDEX after CREATE, found %s", p.at().Describe())
	}
}

// createTable parses the rest of a CREATE TABLE statement.
func (p *parser) createTable() (Statement, *Error) {
	exists, err := p.ifNotExists()
	if err != nil {
		return nil, err
	}
	name, err := p.expectName("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectSymbol("(", "after the table name"); err != nil {
		return nil, err
	}
	statement := &CreateTable{Table: name, IfNotExists: exists}
	for {
		column, err := p.columnDef()
		if err != nil {
			return nil, err
		}
		statement.Columns = append(statement.Columns, column)
		if p.acceptSymbol(",") {
			continue
		}
		break
	}
	if err := p.expectSymbol(")", "after the column list"); err != nil {
		return nil, err
	}
	return statement, nil
}

// columnDef parses one column declaration and its constraints.
func (p *parser) columnDef() (ColumnDef, *Error) {
	name, err := p.expectName("a column name")
	if err != nil {
		return ColumnDef{}, err
	}
	typeToken := p.at()
	if typeToken.Kind != KindKeyword && typeToken.Kind != KindIdent {
		return ColumnDef{}, p.errorHere("expected a type for column %s, found %s", name, typeToken.Describe())
	}
	kind, ok := value.ParseType(typeToken.Lexeme)
	if !ok {
		return ColumnDef{}, p.errorHere("unknown column type %s; emberdb has INTEGER, REAL, TEXT and BLOB", typeToken.Lexeme)
	}
	p.advance()

	column := ColumnDef{Name: name, Type: kind}
	for {
		switch {
		case p.acceptKeyword("PRIMARY"):
			if err := p.expectKeyword("KEY", "after PRIMARY"); err != nil {
				return ColumnDef{}, err
			}
			column.PrimaryKey = true
			column.NotNull = true
		case p.acceptKeyword("UNIQUE"):
			column.Unique = true
		case p.acceptKeyword("NOT"):
			if !p.acceptNull() {
				return ColumnDef{}, p.errorHere("expected NULL after NOT in a column constraint, found %s", p.at().Describe())
			}
			column.NotNull = true
		case p.acceptNull():
			// A bare NULL constraint, which is the default anyway.
		default:
			return column, nil
		}
	}
}

// createIndex parses the rest of a CREATE INDEX statement.
func (p *parser) createIndex(unique bool) (Statement, *Error) {
	p.advance() // INDEX
	exists, err := p.ifNotExists()
	if err != nil {
		return nil, err
	}
	name, err := p.expectName("an index name")
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("ON", "after the index name"); err != nil {
		return nil, err
	}
	table, err := p.expectName("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectSymbol("(", "after the table name"); err != nil {
		return nil, err
	}
	column, err := p.expectName("a column name")
	if err != nil {
		return nil, err
	}
	if p.at().is(KindSymbol, ",") {
		return nil, p.errorHere("emberdb indexes a single column; %s cannot be indexed together with another", column)
	}
	if err := p.expectSymbol(")", "after the indexed column"); err != nil {
		return nil, err
	}
	return &CreateIndex{Name: name, Table: table, Column: column, Unique: unique, IfNotExists: exists}, nil
}

// insertStatement parses INSERT INTO ... VALUES ...
func (p *parser) insertStatement() (Statement, *Error) {
	p.advance() // INSERT
	if err := p.expectKeyword("INTO", "after INSERT"); err != nil {
		return nil, err
	}
	table, err := p.expectName("a table name")
	if err != nil {
		return nil, err
	}
	statement := &Insert{Table: table}
	if p.acceptSymbol("(") {
		for {
			column, err := p.expectName("a column name")
			if err != nil {
				return nil, err
			}
			statement.Columns = append(statement.Columns, column)
			if p.acceptSymbol(",") {
				continue
			}
			break
		}
		if err := p.expectSymbol(")", "after the column list"); err != nil {
			return nil, err
		}
	}
	if err := p.expectKeyword("VALUES", "after the table name"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectSymbol("(", "before a row of values"); err != nil {
			return nil, err
		}
		var row []Expr
		for {
			expr, err := p.expression()
			if err != nil {
				return nil, err
			}
			row = append(row, expr)
			if p.acceptSymbol(",") {
				continue
			}
			break
		}
		if err := p.expectSymbol(")", "after a row of values"); err != nil {
			return nil, err
		}
		statement.Rows = append(statement.Rows, row)
		if p.acceptSymbol(",") {
			continue
		}
		break
	}
	return statement, nil
}

// selectStatement parses SELECT ... FROM ... with its optional clauses.
func (p *parser) selectStatement() (Statement, *Error) {
	p.advance() // SELECT
	statement := &Select{}
	if p.acceptSymbol("*") {
		statement.Star = true
	} else {
		for {
			expr, err := p.expression()
			if err != nil {
				return nil, err
			}
			column := ResultColumn{Expr: expr}
			if p.acceptKeyword("AS") {
				alias, err := p.expectName("an alias after AS")
				if err != nil {
					return nil, err
				}
				column.Alias = alias
			} else if p.at().Kind == KindIdent {
				column.Alias = p.advance().Lexeme
			}
			statement.Columns = append(statement.Columns, column)
			if p.acceptSymbol(",") {
				continue
			}
			break
		}
	}
	if err := p.expectKeyword("FROM", "after the select list"); err != nil {
		return nil, err
	}
	table, err := p.expectName("a table name")
	if err != nil {
		return nil, err
	}
	statement.Table = table

	if p.acceptKeyword("WHERE") {
		statement.Where, err = p.expression()
		if err != nil {
			return nil, err
		}
	}
	if p.acceptKeyword("ORDER") {
		if err := p.expectKeyword("BY", "after ORDER"); err != nil {
			return nil, err
		}
		for {
			expr, err := p.expression()
			if err != nil {
				return nil, err
			}
			term := OrderTerm{Expr: expr}
			switch {
			case p.acceptKeyword("DESC"):
				term.Descending = true
			case p.acceptKeyword("ASC"):
			}
			statement.OrderBy = append(statement.OrderBy, term)
			if p.acceptSymbol(",") {
				continue
			}
			break
		}
	}
	if p.acceptKeyword("LIMIT") {
		statement.Limit, err = p.expression()
		if err != nil {
			return nil, err
		}
		if p.acceptKeyword("OFFSET") {
			statement.Offset, err = p.expression()
			if err != nil {
				return nil, err
			}
		}
	}
	return statement, nil
}

// updateStatement parses UPDATE ... SET ... [WHERE ...]
func (p *parser) updateStatement() (Statement, *Error) {
	p.advance() // UPDATE
	table, err := p.expectName("a table name")
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("SET", "after the table name"); err != nil {
		return nil, err
	}
	statement := &Update{Table: table}
	for {
		column, err := p.expectName("a column name")
		if err != nil {
			return nil, err
		}
		if err := p.expectSymbol("=", "after the column name"); err != nil {
			return nil, err
		}
		expr, exprErr := p.expression()
		if exprErr != nil {
			return nil, exprErr
		}
		statement.Assignments = append(statement.Assignments, Assignment{Column: column, Value: expr})
		if p.acceptSymbol(",") {
			continue
		}
		break
	}
	if p.acceptKeyword("WHERE") {
		where, err := p.expression()
		if err != nil {
			return nil, err
		}
		statement.Where = where
	}
	return statement, nil
}

// deleteStatement parses DELETE FROM ... [WHERE ...]
func (p *parser) deleteStatement() (Statement, *Error) {
	p.advance() // DELETE
	if err := p.expectKeyword("FROM", "after DELETE"); err != nil {
		return nil, err
	}
	table, err := p.expectName("a table name")
	if err != nil {
		return nil, err
	}
	statement := &Delete{Table: table}
	if p.acceptKeyword("WHERE") {
		where, err := p.expression()
		if err != nil {
			return nil, err
		}
		statement.Where = where
	}
	return statement, nil
}
