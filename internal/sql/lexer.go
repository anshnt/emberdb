package sql

import (
	"encoding/hex"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/anshnt/emberdb/internal/value"
)

// lexer walks the query text producing tokens. It tracks line and column so
// that every error can point at the character that caused it.
type lexer struct {
	query  string
	offset int
	line   int
	column int
}

// newLexer returns a lexer positioned at the start of query.
func newLexer(query string) *lexer {
	return &lexer{query: query, line: 1, column: 1}
}

// multi-character symbols, longest first so that ">=" is not read as ">".
var symbols = []string{"<>", "<=", ">=", "!=", "==", "||", "(", ")", ",", ";", ".", "*", "+", "-", "/", "%", "<", ">", "="}

// next returns the next token, or an error for input that cannot be a token at
// all.
func (l *lexer) next() (Token, *Error) {
	l.skipSpaceAndComments()
	start := l.mark()
	if l.offset >= len(l.query) {
		return start, nil
	}
	c := l.query[l.offset]
	switch {
	case c == '\'':
		return l.lexString(start)
	case (c == 'x' || c == 'X') && l.peekAt(1) == '\'':
		return l.lexBlob(start)
	case c == '"':
		return l.lexQuotedIdent(start)
	case c >= '0' && c <= '9':
		return l.lexNumber(start)
	case c == '.' && isDigit(l.peekAt(1)):
		return l.lexNumber(start)
	case isIdentStart(c):
		return l.lexWord(start), nil
	}
	for _, sym := range symbols {
		if strings.HasPrefix(l.query[l.offset:], sym) {
			l.advance(len(sym))
			start.Kind, start.Lexeme = KindSymbol, sym
			return start, nil
		}
	}
	r, size := utf8.DecodeRuneInString(l.query[l.offset:])
	l.advance(size)
	return start, &Error{
		Message: "unexpected character " + strconv.QuoteRune(r),
		Line:    start.Line,
		Column:  start.Column,
		Query:   l.query,
	}
}

// mark returns a token stamped with the current position.
func (l *lexer) mark() Token {
	return Token{Kind: KindEOF, Offset: l.offset, Line: l.line, Column: l.column}
}

// advance moves forward n bytes, keeping the line and column current.
func (l *lexer) advance(n int) {
	for i := 0; i < n && l.offset < len(l.query); i++ {
		if l.query[l.offset] == '\n' {
			l.line++
			l.column = 1
		} else {
			l.column++
		}
		l.offset++
	}
}

// peekAt returns the byte n positions ahead, or zero past the end.
func (l *lexer) peekAt(n int) byte {
	if l.offset+n >= len(l.query) {
		return 0
	}
	return l.query[l.offset+n]
}

// skipSpaceAndComments consumes whitespace, line comments and block comments.
func (l *lexer) skipSpaceAndComments() {
	for l.offset < len(l.query) {
		c := l.query[l.offset]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			l.advance(1)
		case c == '-' && l.peekAt(1) == '-':
			for l.offset < len(l.query) && l.query[l.offset] != '\n' {
				l.advance(1)
			}
		case c == '/' && l.peekAt(1) == '*':
			l.advance(2)
			for l.offset < len(l.query) && !(l.query[l.offset] == '*' && l.peekAt(1) == '/') {
				l.advance(1)
			}
			l.advance(2)
		default:
			return
		}
	}
}

// lexWord reads a bare identifier or a keyword.
func (l *lexer) lexWord(start Token) Token {
	begin := l.offset
	for l.offset < len(l.query) && isIdentPart(l.query[l.offset]) {
		l.advance(1)
	}
	word := l.query[begin:l.offset]
	if upper := strings.ToUpper(word); keywords[upper] {
		start.Kind, start.Lexeme = KindKeyword, upper
		switch upper {
		case "NULL":
			start.Kind, start.Value = KindLiteral, value.Null()
		case "TRUE":
			start.Kind, start.Value = KindLiteral, value.Integer(1)
		case "FALSE":
			start.Kind, start.Value = KindLiteral, value.Integer(0)
		}
		return start
	}
	start.Kind, start.Lexeme = KindIdent, word
	return start
}

// lexQuotedIdent reads a "quoted name", which keeps its exact spelling and may
// contain anything, with "" standing for a literal quote.
func (l *lexer) lexQuotedIdent(start Token) (Token, *Error) {
	l.advance(1)
	var b strings.Builder
	for {
		if l.offset >= len(l.query) {
			return start, &Error{Message: "unterminated quoted name", Line: start.Line, Column: start.Column, Query: l.query, Unterminated: true}
		}
		if l.query[l.offset] == '"' {
			if l.peekAt(1) == '"' {
				b.WriteByte('"')
				l.advance(2)
				continue
			}
			l.advance(1)
			start.Kind, start.Lexeme = KindIdent, b.String()
			return start, nil
		}
		b.WriteByte(l.query[l.offset])
		l.advance(1)
	}
}

// lexString reads a 'text literal', with ” standing for a literal quote.
func (l *lexer) lexString(start Token) (Token, *Error) {
	l.advance(1)
	var b strings.Builder
	for {
		if l.offset >= len(l.query) {
			return start, &Error{Message: "unterminated string literal", Line: start.Line, Column: start.Column, Query: l.query, Unterminated: true}
		}
		if l.query[l.offset] == '\'' {
			if l.peekAt(1) == '\'' {
				b.WriteByte('\'')
				l.advance(2)
				continue
			}
			l.advance(1)
			start.Kind, start.Value = KindLiteral, value.Text(b.String())
			return start, nil
		}
		b.WriteByte(l.query[l.offset])
		l.advance(1)
	}
}

// lexBlob reads an x'..' blob literal.
func (l *lexer) lexBlob(start Token) (Token, *Error) {
	l.advance(2)
	begin := l.offset
	for l.offset < len(l.query) && l.query[l.offset] != '\'' {
		l.advance(1)
	}
	if l.offset >= len(l.query) {
		return start, &Error{Message: "unterminated blob literal", Line: start.Line, Column: start.Column, Query: l.query, Unterminated: true}
	}
	digits := l.query[begin:l.offset]
	l.advance(1)
	decoded, err := hex.DecodeString(digits)
	if err != nil {
		return start, &Error{
			Message: "blob literal must be an even number of hexadecimal digits",
			Line:    start.Line, Column: start.Column, Query: l.query,
		}
	}
	start.Kind, start.Value = KindLiteral, value.Blob(decoded)
	return start, nil
}

// lexNumber reads an integer or a real. A number with a fraction or an
// exponent is a real; anything else is an integer, and one too large for
// int64 becomes a real rather than an error.
func (l *lexer) lexNumber(start Token) (Token, *Error) {
	begin := l.offset
	isReal := false
	for l.offset < len(l.query) && isDigit(l.query[l.offset]) {
		l.advance(1)
	}
	if l.offset < len(l.query) && l.query[l.offset] == '.' {
		isReal = true
		l.advance(1)
		for l.offset < len(l.query) && isDigit(l.query[l.offset]) {
			l.advance(1)
		}
	}
	if l.offset < len(l.query) && (l.query[l.offset] == 'e' || l.query[l.offset] == 'E') {
		mark := l.offset
		l.advance(1)
		if l.offset < len(l.query) && (l.query[l.offset] == '+' || l.query[l.offset] == '-') {
			l.advance(1)
		}
		if l.offset < len(l.query) && isDigit(l.query[l.offset]) {
			isReal = true
			for l.offset < len(l.query) && isDigit(l.query[l.offset]) {
				l.advance(1)
			}
		} else {
			// Not an exponent after all, for example "1e" or "2early".
			l.offset = mark
			l.column -= l.offset - mark
		}
	}
	text := l.query[begin:l.offset]
	if !isReal {
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			start.Kind, start.Value = KindLiteral, value.Integer(n)
			return start, nil
		}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return start, &Error{Message: "malformed number " + text, Line: start.Line, Column: start.Column, Query: l.query}
	}
	start.Kind, start.Value = KindLiteral, value.Real(f)
	return start, nil
}

// isDigit reports whether b is an ASCII digit.
func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// isIdentStart reports whether b may begin a bare identifier.
func isIdentStart(b byte) bool {
	return b == '_' || b >= utf8.RuneSelf || unicode.IsLetter(rune(b))
}

// isIdentPart reports whether b may continue a bare identifier.
func isIdentPart(b byte) bool { return isIdentStart(b) || isDigit(b) || b == '$' }
