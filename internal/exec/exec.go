package exec

import (
	"errors"
	"fmt"
	"sort"

	"github.com/anshnt/emberdb/internal/sql"
	"github.com/anshnt/emberdb/internal/store"
	"github.com/anshnt/emberdb/internal/value"
)

// ErrNoTransaction reports COMMIT or ROLLBACK outside a transaction.
var ErrNoTransaction = errors.New("emberdb: no transaction is open")

// Result is what running a statement produced.
//
// A statement either returns rows or counts them, never both: Columns and Rows
// are set for a SELECT, and Changed for INSERT, UPDATE and DELETE.
type Result struct {
	// Columns names the result columns of a SELECT.
	Columns []string
	// Rows holds the result rows of a SELECT.
	Rows [][]value.Value
	// Changed counts the rows an INSERT, UPDATE or DELETE touched.
	Changed int
	// LastInsertID is the row id of the last row an INSERT created.
	LastInsertID uint64
	// Plan describes how the statement found its rows, for a SELECT,
	// UPDATE or DELETE.
	Plan string
}

// Run executes a statement inside a transaction.
func Run(tx *store.Tx, statement sql.Statement) (Result, error) {
	switch node := statement.(type) {
	case *sql.CreateTable:
		return runCreateTable(tx, node)
	case *sql.CreateIndex:
		return runCreateIndex(tx, node)
	case *sql.Insert:
		return runInsert(tx, node)
	case *sql.Select:
		return runSelect(tx, node)
	case *sql.Update:
		return runUpdate(tx, node)
	case *sql.Delete:
		return runDelete(tx, node)
	case *sql.Begin, *sql.Commit, *sql.Rollback:
		return Result{}, fmt.Errorf("emberdb: transaction control is handled by the caller, not by Run")
	default:
		return Result{}, fmt.Errorf("emberdb: cannot execute %T", statement)
	}
}

// Writes reports whether a statement modifies the database, which is how a
// caller knows to open a write transaction for it.
func Writes(statement sql.Statement) bool {
	switch statement.(type) {
	case *sql.CreateTable, *sql.CreateIndex, *sql.Insert, *sql.Update, *sql.Delete:
		return true
	default:
		return false
	}
}

func runCreateTable(tx *store.Tx, node *sql.CreateTable) (Result, error) {
	if node.IfNotExists {
		if _, err := tx.Table(node.Table); err == nil {
			return Result{}, nil
		} else if !errors.Is(err, store.ErrNoSuchTable) {
			return Result{}, err
		}
	}
	columns := make([]store.Column, len(node.Columns))
	for i, c := range node.Columns {
		columns[i] = store.Column{
			Name:       c.Name,
			Type:       c.Type,
			NotNull:    c.NotNull,
			PrimaryKey: c.PrimaryKey,
			Unique:     c.Unique,
		}
	}
	_, err := tx.CreateTable(node.Table, columns)
	return Result{}, err
}

func runCreateIndex(tx *store.Tx, node *sql.CreateIndex) (Result, error) {
	table, err := tx.Table(node.Table)
	if err != nil {
		return Result{}, err
	}
	column, ok := table.ColumnIndex(node.Column)
	if !ok {
		return Result{}, fmt.Errorf("%w: %s.%s", ErrNoSuchColumn, node.Table, node.Column)
	}
	if node.IfNotExists {
		for _, existing := range table.Indexes {
			if store.EqualNames(existing.Name, node.Name) {
				return Result{}, nil
			}
		}
	}
	return Result{}, tx.CreateIndex(table, node.Name, column, node.Unique)
}

func runInsert(tx *store.Tx, node *sql.Insert) (Result, error) {
	table, err := tx.Table(node.Table)
	if err != nil {
		return Result{}, err
	}
	// Map the statement's column list onto the table's columns. Without a
	// list the values are positional.
	positions := make([]int, len(table.Columns))
	for i := range positions {
		positions[i] = -1
	}
	if len(node.Columns) == 0 {
		for i := range table.Columns {
			positions[i] = i
		}
	} else {
		for valueIndex, name := range node.Columns {
			column, ok := table.ColumnIndex(name)
			if !ok {
				return Result{}, fmt.Errorf("%w: %s.%s", ErrNoSuchColumn, table.Name, name)
			}
			if positions[column] != -1 {
				return Result{}, fmt.Errorf("emberdb: column %s is assigned twice", table.Columns[column].Name)
			}
			positions[column] = valueIndex
		}
	}

	result := Result{}
	empty := &scope{table: table, columns: make([]value.Value, len(table.Columns))}
	for _, exprRow := range node.Rows {
		expected := len(table.Columns)
		if len(node.Columns) > 0 {
			expected = len(node.Columns)
		}
		if len(exprRow) != expected {
			return result, fmt.Errorf("emberdb: table %s expects %d values, got %d", table.Name, expected, len(exprRow))
		}
		row := make([]value.Value, len(table.Columns))
		for column := range row {
			if positions[column] == -1 {
				row[column] = value.Null()
				continue
			}
			// A value expression sees no row, so a column reference in it
			// is an error rather than a silent null.
			v, err := evaluate(exprRow[positions[column]], empty)
			if err != nil {
				return result, err
			}
			row[column] = v
		}
		id, err := tx.Insert(table, row)
		if err != nil {
			return result, err
		}
		result.Changed++
		result.LastInsertID = id
	}
	return result, nil
}

func runSelect(tx *store.Tx, node *sql.Select) (Result, error) {
	table, err := tx.Table(node.Table)
	if err != nil {
		return Result{}, err
	}
	chosen := planScan(table, node.Where)
	result := Result{Plan: chosen.description}

	// Work out the output shape before reading anything, so that a bad
	// column name fails before the scan rather than partway through it.
	projections := node.Columns
	if node.Star {
		projections = make([]sql.ResultColumn, len(table.Columns))
		for i, c := range table.Columns {
			projections[i] = sql.ResultColumn{
				Expr:  &sql.ColumnRef{Name: c.Name},
				Alias: c.Name,
			}
		}
	}
	for _, projection := range projections {
		result.Columns = append(result.Columns, columnName(projection))
	}

	rows, err := chosen.rows(tx, table)
	if err != nil {
		return result, err
	}
	// ORDER BY keys are computed alongside the row so that the sort does
	// not have to re-evaluate expressions for every comparison.
	type sortable struct {
		values []value.Value
		keys   []value.Value
		id     uint64
	}
	var collected []sortable
	s := &scope{table: table}
	for rows.Next() {
		s.columns = rows.Row().Values
		keep, err := matches(node.Where, s)
		if err != nil {
			return result, err
		}
		if !keep {
			continue
		}
		projected := make([]value.Value, len(projections))
		for i, projection := range projections {
			v, err := evaluate(projection.Expr, s)
			if err != nil {
				return result, err
			}
			projected[i] = v
		}
		entry := sortable{values: projected, id: rows.Row().ID}
		for _, term := range node.OrderBy {
			v, err := evaluate(term.Expr, s)
			if err != nil {
				return result, err
			}
			entry.keys = append(entry.keys, v)
		}
		collected = append(collected, entry)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}

	if len(node.OrderBy) > 0 {
		sort.SliceStable(collected, func(i, j int) bool {
			for k, term := range node.OrderBy {
				c := value.Compare(collected[i].keys[k], collected[j].keys[k])
				if term.Descending {
					c = -c
				}
				if c != 0 {
					return c < 0
				}
			}
			// Ties keep row-id order so that results are reproducible.
			return collected[i].id < collected[j].id
		})
	}

	offset, limit, err := limits(node)
	if err != nil {
		return result, err
	}
	if offset > len(collected) {
		offset = len(collected)
	}
	collected = collected[offset:]
	if limit >= 0 && limit < len(collected) {
		collected = collected[:limit]
	}
	result.Rows = make([][]value.Value, len(collected))
	for i, entry := range collected {
		result.Rows[i] = entry.values
	}
	return result, nil
}

// columnName picks the heading for a result column.
func columnName(projection sql.ResultColumn) string {
	if projection.Alias != "" {
		return projection.Alias
	}
	if ref, ok := projection.Expr.(*sql.ColumnRef); ok {
		return ref.Name
	}
	return "?column?"
}

// limits evaluates LIMIT and OFFSET, which are constant expressions.
func limits(node *sql.Select) (offset, limit int, err error) {
	limit = -1
	empty := &scope{table: &store.Table{}}
	if node.Limit != nil {
		v, err := evaluate(node.Limit, empty)
		if err != nil {
			return 0, 0, err
		}
		if v.Kind() != value.TypeInteger || v.Int() < 0 {
			return 0, 0, fmt.Errorf("emberdb: LIMIT must be a non-negative integer, got %s", v.SQL())
		}
		limit = int(v.Int())
	}
	if node.Offset != nil {
		v, err := evaluate(node.Offset, empty)
		if err != nil {
			return 0, 0, err
		}
		if v.Kind() != value.TypeInteger || v.Int() < 0 {
			return 0, 0, fmt.Errorf("emberdb: OFFSET must be a non-negative integer, got %s", v.SQL())
		}
		offset = int(v.Int())
	}
	return offset, limit, nil
}

// matches evaluates a WHERE clause. A null result keeps no row: SQL only
// admits rows the predicate is definitely true for.
func matches(where sql.Expr, s *scope) (bool, error) {
	if where == nil {
		return true, nil
	}
	v, err := evaluate(where, s)
	if err != nil {
		return false, err
	}
	return !v.IsNull() && v.Truthy(), nil
}

func runUpdate(tx *store.Tx, node *sql.Update) (Result, error) {
	table, err := tx.Table(node.Table)
	if err != nil {
		return Result{}, err
	}
	assignments := make([]int, len(node.Assignments))
	for i, assignment := range node.Assignments {
		column, ok := table.ColumnIndex(assignment.Column)
		if !ok {
			return Result{}, fmt.Errorf("%w: %s.%s", ErrNoSuchColumn, table.Name, assignment.Column)
		}
		assignments[i] = column
	}

	chosen := planScan(table, node.Where)
	result := Result{Plan: chosen.description}
	// Collect the target rows before writing any of them: modifying rows
	// while iterating the tree they live in would be reading a structure
	// out from under itself.
	targets, err := collectMatching(tx, table, node.Where, chosen)
	if err != nil {
		return result, err
	}
	s := &scope{table: table}
	for _, target := range targets {
		s.columns = target.Values
		updated := make([]value.Value, len(table.Columns))
		copy(updated, target.Values)
		for i, assignment := range node.Assignments {
			// Each assignment sees the row as it was, so
			// "SET a = b, b = a" swaps rather than duplicates.
			v, err := evaluate(assignment.Value, s)
			if err != nil {
				return result, err
			}
			updated[assignments[i]] = v
		}
		changed, err := tx.Update(table, target.ID, updated)
		if err != nil {
			return result, err
		}
		if changed {
			result.Changed++
		}
	}
	return result, nil
}

func runDelete(tx *store.Tx, node *sql.Delete) (Result, error) {
	table, err := tx.Table(node.Table)
	if err != nil {
		return Result{}, err
	}
	chosen := planScan(table, node.Where)
	result := Result{Plan: chosen.description}
	targets, err := collectMatching(tx, table, node.Where, chosen)
	if err != nil {
		return result, err
	}
	for _, target := range targets {
		removed, err := tx.Delete(table, target.ID)
		if err != nil {
			return result, err
		}
		if removed {
			result.Changed++
		}
	}
	return result, nil
}

// collectMatching gathers the rows a WHERE clause selects.
func collectMatching(tx *store.Tx, table *store.Table, where sql.Expr, chosen plan) ([]store.Row, error) {
	rows, err := chosen.rows(tx, table)
	if err != nil {
		return nil, err
	}
	var targets []store.Row
	s := &scope{table: table}
	for rows.Next() {
		s.columns = rows.Row().Values
		keep, err := matches(where, s)
		if err != nil {
			return nil, err
		}
		if !keep {
			continue
		}
		row := store.Row{ID: rows.Row().ID, Values: append([]value.Value(nil), rows.Row().Values...)}
		targets = append(targets, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return targets, nil
}
