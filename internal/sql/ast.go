package sql

import "github.com/anshnt/emberdb/internal/value"

// Statement is one parsed SQL statement.
type Statement interface {
	statementNode()
}

// ColumnDef is a column as CREATE TABLE declares it.
type ColumnDef struct {
	// Name is the column's name.
	Name string
	// Type is the declared storage class.
	Type value.Type
	// NotNull rejects nulls in this column.
	NotNull bool
	// PrimaryKey marks the column as the primary key.
	PrimaryKey bool
	// Unique requires distinct values.
	Unique bool
}

// CreateTable is a CREATE TABLE statement.
type CreateTable struct {
	// Table is the name to create.
	Table string
	// IfNotExists suppresses the error when the table already exists.
	IfNotExists bool
	// Columns are the declared columns, in order.
	Columns []ColumnDef
}

// CreateIndex is a CREATE INDEX statement.
type CreateIndex struct {
	// Name is the index's name.
	Name string
	// Table is the table to index.
	Table string
	// Column is the column to index.
	Column string
	// Unique makes the index reject duplicate values.
	Unique bool
	// IfNotExists suppresses the error when the index already exists.
	IfNotExists bool
}

// Insert is an INSERT statement, possibly with several rows of values.
type Insert struct {
	// Table is the table to insert into.
	Table string
	// Columns names the columns the values correspond to. It is nil when
	// the statement did not list any, meaning every column in order.
	Columns []string
	// Rows are the rows of value expressions.
	Rows [][]Expr
}

// ResultColumn is one entry in a SELECT list.
type ResultColumn struct {
	// Expr is the expression to evaluate.
	Expr Expr
	// Alias is the name to report the column under, if one was given.
	Alias string
}

// OrderTerm is one entry in an ORDER BY clause.
type OrderTerm struct {
	// Expr is the expression to sort on.
	Expr Expr
	// Descending reverses the sort for this term.
	Descending bool
}

// Select is a SELECT statement.
type Select struct {
	// Table is the table to read.
	Table string
	// Star is set when the statement selected everything with '*'.
	Star bool
	// Columns are the result columns, empty when Star is set.
	Columns []ResultColumn
	// Where filters rows, or is nil.
	Where Expr
	// OrderBy sorts the result, or is empty.
	OrderBy []OrderTerm
	// Limit caps the number of rows, or is nil.
	Limit Expr
	// Offset skips rows before the limit applies, or is nil.
	Offset Expr
}

// Assignment is one SET clause of an UPDATE.
type Assignment struct {
	// Column is the column to write.
	Column string
	// Value is the expression to write into it.
	Value Expr
}

// Update is an UPDATE statement.
type Update struct {
	// Table is the table to modify.
	Table string
	// Assignments are the columns to write and what to write.
	Assignments []Assignment
	// Where selects the rows to modify, or is nil for all of them.
	Where Expr
}

// Delete is a DELETE statement.
type Delete struct {
	// Table is the table to delete from.
	Table string
	// Where selects the rows to delete, or is nil for all of them.
	Where Expr
}

// Begin starts an explicit transaction.
type Begin struct{}

// Commit ends an explicit transaction, keeping its changes.
type Commit struct{}

// Rollback ends an explicit transaction, discarding its changes.
type Rollback struct{}

func (*CreateTable) statementNode() {}
func (*CreateIndex) statementNode() {}
func (*Insert) statementNode()      {}
func (*Select) statementNode()      {}
func (*Update) statementNode()      {}
func (*Delete) statementNode()      {}
func (*Begin) statementNode()       {}
func (*Commit) statementNode()      {}
func (*Rollback) statementNode()    {}

// Expr is a scalar expression.
type Expr interface {
	exprNode()
	// Pos returns the line and column the expression starts at, so that a
	// runtime failure can point at it the way a parse error does.
	Pos() (line, column int)
}

// position is embedded in every expression node.
type position struct {
	Line   int
	Column int
}

// Pos returns the node's source position.
func (p position) Pos() (int, int) { return p.Line, p.Column }

// Literal is a constant.
type Literal struct {
	position
	// Value is the constant.
	Value value.Value
}

// ColumnRef names a column.
type ColumnRef struct {
	position
	// Name is the column's name as written.
	Name string
}

// Unary applies a prefix operator: '-', '+' or NOT.
type Unary struct {
	position
	// Op is the operator.
	Op string
	// Operand is what it applies to.
	Operand Expr
}

// Binary applies an infix operator.
type Binary struct {
	position
	// Op is the operator, normalised so that "==" is "=" and "<>" is "!=".
	Op string
	// Left and Right are the operands.
	Left, Right Expr
}

// IsNull tests for null, optionally negated by IS NOT NULL.
type IsNull struct {
	position
	// Operand is the expression to test.
	Operand Expr
	// Negated turns the test into IS NOT NULL.
	Negated bool
}

// Between tests a range, optionally negated by NOT BETWEEN.
type Between struct {
	position
	// Operand is the value to test.
	Operand Expr
	// Low and High are the inclusive bounds.
	Low, High Expr
	// Negated turns the test into NOT BETWEEN.
	Negated bool
}

// In tests membership of a list, optionally negated by NOT IN.
type In struct {
	position
	// Operand is the value to test.
	Operand Expr
	// List is what to test against.
	List []Expr
	// Negated turns the test into NOT IN.
	Negated bool
}

// Like matches a pattern where '%' stands for any run of characters and '_'
// for exactly one.
type Like struct {
	position
	// Operand is the string to match.
	Operand Expr
	// Pattern is the pattern to match against.
	Pattern Expr
	// Negated turns the test into NOT LIKE.
	Negated bool
}

func (*Literal) exprNode()   {}
func (*ColumnRef) exprNode() {}
func (*Unary) exprNode()     {}
func (*Binary) exprNode()    {}
func (*IsNull) exprNode()    {}
func (*Between) exprNode()   {}
func (*In) exprNode()        {}
func (*Like) exprNode()      {}
