// Package exec plans and runs parsed SQL statements against the store.
//
// The planner is small on purpose: a statement reads one table, so the only
// real decision is whether a WHERE clause can be answered through an index
// instead of a full scan.
package exec

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/anshnt/emberdb/internal/sql"
	"github.com/anshnt/emberdb/internal/store"
	"github.com/anshnt/emberdb/internal/value"
)

// ErrNoSuchColumn reports a reference to a column the table does not have.
var ErrNoSuchColumn = errors.New("emberdb: no such column")

// ErrEvaluation reports an expression that cannot be evaluated, such as
// division by zero or arithmetic on text.
var ErrEvaluation = errors.New("emberdb: cannot evaluate expression")

// scope resolves column names to positions in a row.
type scope struct {
	table   *store.Table
	columns []value.Value
}

// lookup returns the value of a named column.
func (s *scope) lookup(name string) (value.Value, error) {
	i, ok := s.table.ColumnIndex(name)
	if !ok {
		return value.Null(), fmt.Errorf("%w: %s.%s", ErrNoSuchColumn, s.table.Name, name)
	}
	return s.columns[i], nil
}

// evaluate computes an expression against a row.
//
// Null propagates the way SQL says it does: any arithmetic or comparison
// involving null yields null, and a WHERE clause treats null as "no", so a row
// is only kept when the predicate is definitely true.
func evaluate(expr sql.Expr, s *scope) (value.Value, error) {
	switch node := expr.(type) {
	case *sql.Literal:
		return node.Value, nil
	case *sql.ColumnRef:
		return s.lookup(node.Name)
	case *sql.Unary:
		return evaluateUnary(node, s)
	case *sql.Binary:
		return evaluateBinary(node, s)
	case *sql.IsNull:
		v, err := evaluate(node.Operand, s)
		if err != nil {
			return value.Null(), err
		}
		return boolean(v.IsNull() != node.Negated), nil
	case *sql.Between:
		return evaluateBetween(node, s)
	case *sql.In:
		return evaluateIn(node, s)
	case *sql.Like:
		return evaluateLike(node, s)
	default:
		return value.Null(), fmt.Errorf("%w: unsupported expression %T", ErrEvaluation, expr)
	}
}

// boolean renders a Go bool as SQL's integer truth value.
func boolean(b bool) value.Value {
	if b {
		return value.Integer(1)
	}
	return value.Integer(0)
}

func evaluateUnary(node *sql.Unary, s *scope) (value.Value, error) {
	operand, err := evaluate(node.Operand, s)
	if err != nil {
		return value.Null(), err
	}
	switch node.Op {
	case "NOT":
		if operand.IsNull() {
			return value.Null(), nil
		}
		return boolean(!operand.Truthy()), nil
	case "-":
		switch operand.Kind() {
		case value.TypeNull:
			return value.Null(), nil
		case value.TypeInteger:
			return value.Integer(-operand.Int()), nil
		case value.TypeReal:
			return value.Real(-operand.Float()), nil
		}
		return value.Null(), positioned(node, "%w: cannot negate %s", ErrEvaluation, operand.Kind())
	default:
		return value.Null(), positioned(node, "%w: unknown operator %s", ErrEvaluation, node.Op)
	}
}

func evaluateBinary(node *sql.Binary, s *scope) (value.Value, error) {
	// AND and OR short-circuit, so the right side is only evaluated when it
	// can still change the answer.
	switch node.Op {
	case "AND":
		left, err := evaluate(node.Left, s)
		if err != nil {
			return value.Null(), err
		}
		if !left.IsNull() && !left.Truthy() {
			return boolean(false), nil
		}
		right, err := evaluate(node.Right, s)
		if err != nil {
			return value.Null(), err
		}
		if !right.IsNull() && !right.Truthy() {
			return boolean(false), nil
		}
		if left.IsNull() || right.IsNull() {
			return value.Null(), nil
		}
		return boolean(true), nil
	case "OR":
		left, err := evaluate(node.Left, s)
		if err != nil {
			return value.Null(), err
		}
		if !left.IsNull() && left.Truthy() {
			return boolean(true), nil
		}
		right, err := evaluate(node.Right, s)
		if err != nil {
			return value.Null(), err
		}
		if !right.IsNull() && right.Truthy() {
			return boolean(true), nil
		}
		if left.IsNull() || right.IsNull() {
			return value.Null(), nil
		}
		return boolean(false), nil
	}

	left, err := evaluate(node.Left, s)
	if err != nil {
		return value.Null(), err
	}
	right, err := evaluate(node.Right, s)
	if err != nil {
		return value.Null(), err
	}
	if left.IsNull() || right.IsNull() {
		return value.Null(), nil
	}
	switch node.Op {
	case "=", "!=", "<", "<=", ">", ">=":
		return boolean(compareOp(node.Op, value.Compare(left, right))), nil
	case "||":
		return value.Text(left.String() + right.String()), nil
	}
	return arithmetic(node, left, right)
}

// compareOp turns a comparison result into the answer for an operator.
func compareOp(op string, c int) bool {
	switch op {
	case "=":
		return c == 0
	case "!=":
		return c != 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	default:
		return c >= 0
	}
}

// arithmetic applies +, -, *, / and % to two non-null values.
func arithmetic(node *sql.Binary, left, right value.Value) (value.Value, error) {
	if !left.IsNumeric() || !right.IsNumeric() {
		return value.Null(), positioned(node, "%w: %s needs numbers, got %s and %s",
			ErrEvaluation, node.Op, left.Kind(), right.Kind())
	}
	if left.Kind() == value.TypeInteger && right.Kind() == value.TypeInteger {
		a, b := left.Int(), right.Int()
		switch node.Op {
		case "+":
			return value.Integer(a + b), nil
		case "-":
			return value.Integer(a - b), nil
		case "*":
			return value.Integer(a * b), nil
		case "/":
			if b == 0 {
				return value.Null(), positioned(node, "%w: division by zero", ErrEvaluation)
			}
			return value.Integer(a / b), nil
		case "%":
			if b == 0 {
				return value.Null(), positioned(node, "%w: division by zero", ErrEvaluation)
			}
			return value.Integer(a % b), nil
		}
	}
	a, b := left.Float(), right.Float()
	switch node.Op {
	case "+":
		return value.Real(a + b), nil
	case "-":
		return value.Real(a - b), nil
	case "*":
		return value.Real(a * b), nil
	case "/":
		if b == 0 {
			return value.Null(), positioned(node, "%w: division by zero", ErrEvaluation)
		}
		return value.Real(a / b), nil
	case "%":
		if b == 0 {
			return value.Null(), positioned(node, "%w: division by zero", ErrEvaluation)
		}
		return value.Real(math.Mod(a, b)), nil
	}
	return value.Null(), positioned(node, "%w: unknown operator %s", ErrEvaluation, node.Op)
}

func evaluateBetween(node *sql.Between, s *scope) (value.Value, error) {
	operand, err := evaluate(node.Operand, s)
	if err != nil {
		return value.Null(), err
	}
	low, err := evaluate(node.Low, s)
	if err != nil {
		return value.Null(), err
	}
	high, err := evaluate(node.High, s)
	if err != nil {
		return value.Null(), err
	}
	if operand.IsNull() || low.IsNull() || high.IsNull() {
		return value.Null(), nil
	}
	within := value.Compare(operand, low) >= 0 && value.Compare(operand, high) <= 0
	return boolean(within != node.Negated), nil
}

func evaluateIn(node *sql.In, s *scope) (value.Value, error) {
	operand, err := evaluate(node.Operand, s)
	if err != nil {
		return value.Null(), err
	}
	if operand.IsNull() {
		return value.Null(), nil
	}
	sawNull := false
	for _, item := range node.List {
		candidate, err := evaluate(item, s)
		if err != nil {
			return value.Null(), err
		}
		if candidate.IsNull() {
			sawNull = true
			continue
		}
		if value.Compare(operand, candidate) == 0 {
			return boolean(!node.Negated), nil
		}
	}
	if sawNull {
		// A miss against a list holding null is unknown, not false.
		return value.Null(), nil
	}
	return boolean(node.Negated), nil
}

func evaluateLike(node *sql.Like, s *scope) (value.Value, error) {
	operand, err := evaluate(node.Operand, s)
	if err != nil {
		return value.Null(), err
	}
	pattern, err := evaluate(node.Pattern, s)
	if err != nil {
		return value.Null(), err
	}
	if operand.IsNull() || pattern.IsNull() {
		return value.Null(), nil
	}
	matched := likeMatch(strings.ToLower(operand.String()), strings.ToLower(pattern.String()))
	return boolean(matched != node.Negated), nil
}

// likeMatch implements SQL's LIKE, where '%' stands for any run of characters
// and '_' for exactly one.
//
// The two indexes walk the input together, and on a '%' the pattern position
// is remembered so that a later mismatch can resume one character further into
// the text. That keeps it linear on the patterns people actually write,
// without the backtracking a recursive version would do.
func likeMatch(text, pattern string) bool {
	var (
		t, p         int
		starPattern  = -1
		starText     int
		runes        = []rune(text)
		patternRunes = []rune(pattern)
	)
	for t < len(runes) {
		switch {
		case p < len(patternRunes) && (patternRunes[p] == '_' || patternRunes[p] == runes[t]):
			t++
			p++
		case p < len(patternRunes) && patternRunes[p] == '%':
			starPattern, starText = p, t
			p++
		case starPattern >= 0:
			starText++
			t, p = starText, starPattern+1
		default:
			return false
		}
	}
	for p < len(patternRunes) && patternRunes[p] == '%' {
		p++
	}
	return p == len(patternRunes)
}

// positioned wraps an error with the source position of the expression that
// produced it.
func positioned(expr sql.Expr, format string, args ...any) error {
	line, column := expr.Pos()
	return fmt.Errorf("%w (at line %d, column %d)", fmt.Errorf(format, args...), line, column)
}
