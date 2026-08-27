// Package sql turns SQL text into statements: a hand-written lexer and a
// recursive-descent parser, with no generator involved.
//
// The dialect is a deliberate subset. It covers CREATE TABLE, CREATE INDEX,
// INSERT, SELECT with WHERE, ORDER BY and LIMIT, UPDATE, DELETE and explicit
// transaction control. It has no joins, no subqueries and no aggregates; see
// docs/decisions.md for why.
package sql

import (
	"fmt"

	"github.com/anshnt/emberdb/internal/value"
)

// Kind classifies a token.
type Kind uint8

const (
	// KindEOF marks the end of the input.
	KindEOF Kind = iota
	// KindIdent is a table, column or index name.
	KindIdent
	// KindKeyword is a reserved word, always upper-cased in Lexeme.
	KindKeyword
	// KindLiteral is a number, string, blob or NULL.
	KindLiteral
	// KindSymbol is an operator or a piece of punctuation.
	KindSymbol
)

// String returns the kind's name, as it appears in parse errors.
func (k Kind) String() string {
	switch k {
	case KindEOF:
		return "end of input"
	case KindIdent:
		return "name"
	case KindKeyword:
		return "keyword"
	case KindLiteral:
		return "literal"
	case KindSymbol:
		return "symbol"
	default:
		return "token"
	}
}

// Token is one lexical element together with where it came from.
type Token struct {
	// Kind is what sort of token this is.
	Kind Kind
	// Lexeme is the token's text: the identifier, the upper-cased keyword,
	// or the operator. It is empty for literals.
	Lexeme string
	// Value is the value a literal carries.
	Value value.Value
	// Offset is the byte offset the token starts at in the query.
	Offset int
	// Line is the one-based line the token starts on.
	Line int
	// Column is the one-based column the token starts at.
	Column int
}

// Describe renders a token the way a parse error should name it.
func (t Token) Describe() string {
	switch t.Kind {
	case KindEOF:
		return "end of input"
	case KindLiteral:
		return fmt.Sprintf("literal %s", t.Value.SQL())
	case KindIdent:
		return fmt.Sprintf("name %q", t.Lexeme)
	default:
		return fmt.Sprintf("%q", t.Lexeme)
	}
}

// is reports whether the token is a specific keyword or symbol.
func (t Token) is(kind Kind, lexeme string) bool {
	return t.Kind == kind && t.Lexeme == lexeme
}

// keywords are the reserved words the parser recognises. A name that is not
// here stays an identifier, so a column may be called "value" or "count".
var keywords = map[string]bool{
	"AND": true, "AS": true, "ASC": true, "BEGIN": true, "BETWEEN": true,
	"BLOB": true, "BY": true, "COMMIT": true, "CREATE": true, "DELETE": true,
	"DESC": true, "DOUBLE": true, "DROP": true, "EXISTS": true, "FALSE": true, "FLOAT": true,
	"FROM": true, "IF": true, "IN": true, "INDEX": true, "INSERT": true,
	"INT": true, "INTEGER": true, "INTO": true, "IS": true, "KEY": true,
	"LIKE": true, "LIMIT": true, "NOT": true, "NULL": true, "OFFSET": true,
	"ON": true, "OR": true, "ORDER": true, "PRIMARY": true, "REAL": true,
	"ROLLBACK": true, "SELECT": true, "SET": true, "STRING": true,
	"TABLE": true, "TEXT": true, "TRANSACTION": true, "TRUE": true,
	"UNIQUE": true, "UPDATE": true, "VALUES": true, "VARCHAR": true,
	"WHERE": true,
}
