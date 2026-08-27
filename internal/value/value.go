// Package value defines emberdb's typed value model and the two encodings it
// needs: a compact one for rows on disk, and an order-preserving one for index
// keys, where byte order has to match value order.
package value

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Type is one of emberdb's storage classes.
type Type uint8

const (
	// TypeNull is the type of the null value, which every column admits
	// unless it is declared NOT NULL.
	TypeNull Type = iota
	// TypeInteger is a 64-bit signed integer.
	TypeInteger
	// TypeReal is a 64-bit IEEE 754 float.
	TypeReal
	// TypeText is a UTF-8 string.
	TypeText
	// TypeBlob is an uninterpreted byte string.
	TypeBlob
)

// String returns the type's SQL name.
func (t Type) String() string {
	switch t {
	case TypeNull:
		return "NULL"
	case TypeInteger:
		return "INTEGER"
	case TypeReal:
		return "REAL"
	case TypeText:
		return "TEXT"
	case TypeBlob:
		return "BLOB"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", uint8(t))
	}
}

// ParseType maps a SQL type name to a Type, case-insensitively.
func ParseType(name string) (Type, bool) {
	switch strings.ToUpper(name) {
	case "INTEGER", "INT":
		return TypeInteger, true
	case "REAL", "FLOAT", "DOUBLE":
		return TypeReal, true
	case "TEXT", "VARCHAR", "STRING":
		return TypeText, true
	case "BLOB":
		return TypeBlob, true
	default:
		return TypeNull, false
	}
}

// Value is a single typed datum. The zero Value is null.
//
// Values are immutable; the constructors copy any bytes they are given, so a
// caller can reuse its buffers freely.
type Value struct {
	kind Type
	num  int64
	real float64
	data []byte
}

// Null returns the null value.
func Null() Value { return Value{kind: TypeNull} }

// Integer returns an integer value.
func Integer(i int64) Value { return Value{kind: TypeInteger, num: i} }

// Real returns a floating-point value.
func Real(f float64) Value { return Value{kind: TypeReal, real: f} }

// Text returns a text value.
func Text(s string) Value { return Value{kind: TypeText, data: []byte(s)} }

// Blob returns a blob value, copying b.
func Blob(b []byte) Value {
	return Value{kind: TypeBlob, data: append([]byte(nil), b...)}
}

// Kind returns the value's type.
func (v Value) Kind() Type { return v.kind }

// IsNull reports whether the value is null.
func (v Value) IsNull() bool { return v.kind == TypeNull }

// Int returns the integer this value holds, or zero if it is not an integer.
func (v Value) Int() int64 { return v.num }

// Float returns the value as a float64, widening an integer if needed. It
// returns zero for values that are not numeric.
func (v Value) Float() float64 {
	switch v.kind {
	case TypeInteger:
		return float64(v.num)
	case TypeReal:
		return v.real
	default:
		return 0
	}
}

// Str returns the text this value holds, or the empty string if it is not
// text.
func (v Value) Str() string {
	if v.kind != TypeText {
		return ""
	}
	return string(v.data)
}

// Bytes returns the text or blob payload without copying it. Callers must not
// modify the result.
func (v Value) Bytes() []byte { return v.data }

// IsNumeric reports whether the value is an integer or a real.
func (v Value) IsNumeric() bool { return v.kind == TypeInteger || v.kind == TypeReal }

// Truthy reports how the value behaves in a WHERE clause. Null is not truthy,
// which is what makes three-valued logic collapse to "row excluded".
func (v Value) Truthy() bool {
	switch v.kind {
	case TypeNull:
		return false
	case TypeInteger:
		return v.num != 0
	case TypeReal:
		return v.real != 0
	case TypeText, TypeBlob:
		return len(v.data) > 0
	default:
		return false
	}
}

// String renders the value the way the CLI prints it.
func (v Value) String() string {
	switch v.kind {
	case TypeNull:
		return "NULL"
	case TypeInteger:
		return strconv.FormatInt(v.num, 10)
	case TypeReal:
		return formatReal(v.real)
	case TypeText:
		return string(v.data)
	case TypeBlob:
		return fmt.Sprintf("x'%x'", v.data)
	default:
		return "?"
	}
}

// SQL renders the value as a SQL literal, quoting text.
func (v Value) SQL() string {
	if v.kind == TypeText {
		return "'" + strings.ReplaceAll(string(v.data), "'", "''") + "'"
	}
	return v.String()
}

// formatReal prints a float without an exponent where one is not needed, and
// always with a decimal point so that a real is never mistaken for an integer.
func formatReal(f float64) string {
	if math.IsInf(f, 1) {
		return "Inf"
	}
	if math.IsInf(f, -1) {
		return "-Inf"
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// typeRank orders the storage classes for comparison. Integers and reals share
// a rank so that numbers compare numerically rather than by representation.
func typeRank(t Type) int {
	switch t {
	case TypeNull:
		return 0
	case TypeInteger, TypeReal:
		return 1
	case TypeText:
		return 2
	default:
		return 3
	}
}

// Compare orders two values, returning a negative number, zero or a positive
// number as a sorts before, with, or after b.
//
// The order is null first, then numbers, then text, then blobs. Nulls compare
// equal to each other here; the three-valued logic that makes NULL = NULL
// unknown lives in the expression evaluator, not in the sort order, because
// ORDER BY still has to put nulls somewhere.
func Compare(a, b Value) int {
	ra, rb := typeRank(a.kind), typeRank(b.kind)
	if ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch ra {
	case 0:
		return 0
	case 1:
		if a.kind == TypeInteger && b.kind == TypeInteger {
			switch {
			case a.num < b.num:
				return -1
			case a.num > b.num:
				return 1
			default:
				return 0
			}
		}
		af, bf := a.Float(), b.Float()
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		case math.IsNaN(af) && !math.IsNaN(bf):
			return -1
		case !math.IsNaN(af) && math.IsNaN(bf):
			return 1
		default:
			return 0
		}
	default:
		return strings.Compare(string(a.data), string(b.data))
	}
}

// Equal reports whether two values are the same value.
func Equal(a, b Value) bool {
	if a.IsNull() != b.IsNull() {
		return false
	}
	if typeRank(a.kind) != typeRank(b.kind) {
		return false
	}
	return Compare(a, b) == 0
}
