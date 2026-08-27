package exec

import (
	"github.com/anshnt/emberdb/internal/sql"
	"github.com/anshnt/emberdb/internal/store"
	"github.com/anshnt/emberdb/internal/value"
)

// plan says how a statement will find its rows.
type plan struct {
	// index is the index to scan, or nil for a full table scan.
	index *store.Index
	// rng bounds the index scan.
	rng store.Range
	// description is what EXPLAIN would say, and what the CLI's timer
	// reports.
	description string
}

// planScan decides whether a WHERE clause can be answered through an index.
//
// It looks for a conjunction of comparisons against one indexed column with
// constant bounds, which is the shape that actually turns up in practice:
// equality lookups and ranges. Anything else falls back to a full scan, which
// is always correct because the predicate is re-evaluated per row regardless.
func planScan(table *store.Table, where sql.Expr) plan {
	full := plan{description: "scan " + table.Name}
	if where == nil {
		return full
	}
	best := plan{}
	for _, term := range conjuncts(where) {
		column, op, bound, ok := comparisonAgainstColumn(table, term)
		if !ok {
			continue
		}
		index, ok := table.IndexOn(column)
		if !ok {
			continue
		}
		coerced, ok := coerceBound(table.Columns[column], bound)
		if !ok {
			// A bound that does not fit the column's type cannot be
			// compared as bytes, so the index cannot answer it.
			continue
		}
		if best.index != nil && best.index != index {
			continue
		}
		best.index = index
		applyBound(&best.rng, op, coerced)
	}
	if best.index == nil {
		return full
	}
	best.description = "search " + table.Name + " using index " + best.index.Name
	return best
}

// conjuncts flattens the top level of an AND chain, so that each comparison
// can be considered for the index independently.
func conjuncts(where sql.Expr) []sql.Expr {
	binary, ok := where.(*sql.Binary)
	if !ok || binary.Op != "AND" {
		return []sql.Expr{where}
	}
	return append(conjuncts(binary.Left), conjuncts(binary.Right)...)
}

// comparisonAgainstColumn recognises "column op constant" and its mirror
// image, returning the column's ordinal, the operator as seen from the
// column's side, and the constant.
func comparisonAgainstColumn(table *store.Table, expr sql.Expr) (column int, op string, bound value.Value, ok bool) {
	switch node := expr.(type) {
	case *sql.Binary:
		switch node.Op {
		case "=", "<", "<=", ">", ">=":
		default:
			return 0, "", value.Null(), false
		}
		if ref, isRef := node.Left.(*sql.ColumnRef); isRef {
			if literal, isLiteral := node.Right.(*sql.Literal); isLiteral {
				if i, found := table.ColumnIndex(ref.Name); found {
					return i, node.Op, literal.Value, true
				}
			}
		}
		if ref, isRef := node.Right.(*sql.ColumnRef); isRef {
			if literal, isLiteral := node.Left.(*sql.Literal); isLiteral {
				if i, found := table.ColumnIndex(ref.Name); found {
					return i, mirror(node.Op), literal.Value, true
				}
			}
		}
	}
	return 0, "", value.Null(), false
}

// mirror flips a comparison so that the column is on the left.
func mirror(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	default:
		return op
	}
}

// coerceBound fits a literal to a column's type, reporting false when it does
// not fit and the index therefore cannot be used.
func coerceBound(column store.Column, bound value.Value) (value.Value, bool) {
	if bound.IsNull() {
		return bound, false
	}
	if bound.Kind() == column.Type {
		return bound, true
	}
	if column.Type == value.TypeReal && bound.Kind() == value.TypeInteger {
		return value.Real(float64(bound.Int())), true
	}
	return bound, false
}

// applyBound narrows a range with one comparison, keeping the tightest bound
// seen so far.
func applyBound(rng *store.Range, op string, bound value.Value) {
	v := bound
	switch op {
	case "=":
		rng.Low, rng.High = &v, &v
		rng.LowOpen, rng.HighOpen = false, false
	case ">", ">=":
		open := op == ">"
		if rng.Low == nil || value.Compare(v, *rng.Low) > 0 {
			rng.Low, rng.LowOpen = &v, open
		}
	case "<", "<=":
		open := op == "<"
		if rng.High == nil || value.Compare(v, *rng.High) < 0 {
			rng.High, rng.HighOpen = &v, open
		}
	}
}

// rows opens the iterator a plan describes.
func (p plan) rows(tx *store.Tx, table *store.Table) (*store.Rows, error) {
	if p.index == nil {
		return tx.Scan(table)
	}
	return tx.ScanIndex(table, p.index, p.rng)
}
