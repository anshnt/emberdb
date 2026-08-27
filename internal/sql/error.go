package sql

import (
	"fmt"
	"strings"
)

// Error is a lexing or parsing failure, carrying enough position to point at
// the problem rather than just naming it.
type Error struct {
	// Message says what went wrong.
	Message string
	// Line is the one-based line the problem is on.
	Line int
	// Column is the one-based column the problem is at.
	Column int
	// Query is the statement text, kept so that Detail can quote the line.
	Query string
	// Unterminated marks an error caused by input that simply stops early,
	// such as an unclosed string. A REPL reads that as "keep typing"
	// rather than as a mistake.
	Unterminated bool
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("emberdb: syntax error at line %d, column %d: %s", e.Line, e.Column, e.Message)
}

// Detail renders the error with the offending line and a caret under it, which
// is what the CLI prints.
func (e *Error) Detail() string {
	var b strings.Builder
	b.WriteString(e.Error())
	lines := strings.Split(e.Query, "\n")
	if e.Line >= 1 && e.Line <= len(lines) {
		source := strings.ReplaceAll(lines[e.Line-1], "\t", " ")
		b.WriteString("\n  ")
		b.WriteString(source)
		if e.Column >= 1 && e.Column <= len(source)+1 {
			b.WriteString("\n  ")
			b.WriteString(strings.Repeat(" ", e.Column-1))
			b.WriteString("^")
		}
	}
	return b.String()
}

// errorAt builds an Error positioned at a token.
func errorAt(query string, tok Token, format string, args ...any) *Error {
	return &Error{
		Message: fmt.Sprintf(format, args...),
		Line:    tok.Line,
		Column:  tok.Column,
		Query:   query,
	}
}
