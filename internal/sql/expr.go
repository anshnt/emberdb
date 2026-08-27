package sql

import "github.com/anshnt/emberdb/internal/value"

// Expression grammar, loosest binding first:
//
//	or          := and { OR and }
//	and         := not { AND not }
//	not         := [ NOT ] predicate
//	predicate   := sum [ comparison | IS [NOT] NULL | [NOT] BETWEEN | [NOT] IN | [NOT] LIKE ]
//	sum         := product { ( + | - | || ) product }
//	product     := unary { ( * | / | % ) unary }
//	unary       := [ - | + ] primary
//	primary     := literal | column | ( or )

// expression parses a full expression.
func (p *parser) expression() (Expr, *Error) { return p.orExpr() }

// here stamps a node with the current token's position.
func (p *parser) here() position {
	tok := p.at()
	return position{Line: tok.Line, Column: tok.Column}
}

func (p *parser) orExpr() (Expr, *Error) {
	left, err := p.andExpr()
	if err != nil {
		return nil, err
	}
	for {
		at := p.here()
		if !p.acceptKeyword("OR") {
			return left, nil
		}
		right, err := p.andExpr()
		if err != nil {
			return nil, err
		}
		left = &Binary{position: at, Op: "OR", Left: left, Right: right}
	}
}

func (p *parser) andExpr() (Expr, *Error) {
	left, err := p.notExpr()
	if err != nil {
		return nil, err
	}
	for {
		at := p.here()
		if !p.acceptKeyword("AND") {
			return left, nil
		}
		right, err := p.notExpr()
		if err != nil {
			return nil, err
		}
		left = &Binary{position: at, Op: "AND", Left: left, Right: right}
	}
}

func (p *parser) notExpr() (Expr, *Error) {
	at := p.here()
	if p.acceptKeyword("NOT") {
		operand, err := p.notExpr()
		if err != nil {
			return nil, err
		}
		return &Unary{position: at, Op: "NOT", Operand: operand}, nil
	}
	return p.predicate()
}

// comparisons maps the operators that may follow a value, normalising the
// spellings SQL allows for the same thing.
var comparisons = map[string]string{
	"=": "=", "==": "=", "!=": "!=", "<>": "!=",
	"<": "<", "<=": "<=", ">": ">", ">=": ">=",
}

func (p *parser) predicate() (Expr, *Error) {
	left, err := p.sumExpr()
	if err != nil {
		return nil, err
	}
	at := p.here()

	if p.at().Kind == KindSymbol {
		if op, ok := comparisons[p.at().Lexeme]; ok {
			p.advance()
			right, err := p.sumExpr()
			if err != nil {
				return nil, err
			}
			return &Binary{position: at, Op: op, Left: left, Right: right}, nil
		}
	}
	if p.acceptKeyword("IS") {
		negated := p.acceptKeyword("NOT")
		if !p.acceptNull() {
			return nil, p.errorHere("expected NULL after IS, found %s", p.at().Describe())
		}
		return &IsNull{position: at, Operand: left, Negated: negated}, nil
	}

	negated := false
	if p.at().is(KindKeyword, "NOT") {
		switch p.peek(1).Lexeme {
		case "BETWEEN", "IN", "LIKE":
			p.advance()
			negated = true
		default:
			return left, nil
		}
	}
	switch {
	case p.acceptKeyword("BETWEEN"):
		low, err := p.sumExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("AND", "between the bounds of BETWEEN"); err != nil {
			return nil, err
		}
		high, err := p.sumExpr()
		if err != nil {
			return nil, err
		}
		return &Between{position: at, Operand: left, Low: low, High: high, Negated: negated}, nil
	case p.acceptKeyword("IN"):
		if err := p.expectSymbol("(", "after IN"); err != nil {
			return nil, err
		}
		node := &In{position: at, Operand: left, Negated: negated}
		if !p.at().is(KindSymbol, ")") {
			for {
				item, err := p.expression()
				if err != nil {
					return nil, err
				}
				node.List = append(node.List, item)
				if p.acceptSymbol(",") {
					continue
				}
				break
			}
		}
		if err := p.expectSymbol(")", "after the IN list"); err != nil {
			return nil, err
		}
		return node, nil
	case p.acceptKeyword("LIKE"):
		pattern, err := p.sumExpr()
		if err != nil {
			return nil, err
		}
		return &Like{position: at, Operand: left, Pattern: pattern, Negated: negated}, nil
	}
	return left, nil
}

func (p *parser) sumExpr() (Expr, *Error) {
	left, err := p.productExpr()
	if err != nil {
		return nil, err
	}
	for {
		at := p.here()
		tok := p.at()
		if tok.Kind != KindSymbol || (tok.Lexeme != "+" && tok.Lexeme != "-" && tok.Lexeme != "||") {
			return left, nil
		}
		p.advance()
		right, err := p.productExpr()
		if err != nil {
			return nil, err
		}
		left = &Binary{position: at, Op: tok.Lexeme, Left: left, Right: right}
	}
}

func (p *parser) productExpr() (Expr, *Error) {
	left, err := p.unaryExpr()
	if err != nil {
		return nil, err
	}
	for {
		at := p.here()
		tok := p.at()
		if tok.Kind != KindSymbol || (tok.Lexeme != "*" && tok.Lexeme != "/" && tok.Lexeme != "%") {
			return left, nil
		}
		p.advance()
		right, err := p.unaryExpr()
		if err != nil {
			return nil, err
		}
		left = &Binary{position: at, Op: tok.Lexeme, Left: left, Right: right}
	}
}

func (p *parser) unaryExpr() (Expr, *Error) {
	at := p.here()
	tok := p.at()
	if tok.Kind == KindSymbol && (tok.Lexeme == "-" || tok.Lexeme == "+") {
		p.advance()
		operand, err := p.unaryExpr()
		if err != nil {
			return nil, err
		}
		if tok.Lexeme == "+" {
			return operand, nil
		}
		// Fold the sign into a numeric literal so that "-1" is a constant
		// rather than a negation applied at every row.
		if literal, ok := operand.(*Literal); ok {
			switch literal.Value.Kind() {
			case value.TypeInteger:
				return &Literal{position: at, Value: value.Integer(-literal.Value.Int())}, nil
			case value.TypeReal:
				return &Literal{position: at, Value: value.Real(-literal.Value.Float())}, nil
			}
		}
		return &Unary{position: at, Op: "-", Operand: operand}, nil
	}
	return p.primaryExpr()
}

func (p *parser) primaryExpr() (Expr, *Error) {
	at := p.here()
	tok := p.at()
	switch {
	case tok.Kind == KindLiteral:
		p.advance()
		return &Literal{position: at, Value: tok.Value}, nil
	case tok.Kind == KindIdent:
		p.advance()
		// A qualified name such as t.col is accepted and the table part
		// dropped, since a statement reads exactly one table.
		if p.at().is(KindSymbol, ".") {
			p.advance()
			name, err := p.expectName("a column name after the table qualifier")
			if err != nil {
				return nil, err
			}
			return &ColumnRef{position: at, Name: name}, nil
		}
		return &ColumnRef{position: at, Name: tok.Lexeme}, nil
	case tok.is(KindSymbol, "("):
		p.advance()
		inner, err := p.expression()
		if err != nil {
			return nil, err
		}
		if err := p.expectSymbol(")", "to close the parenthesised expression"); err != nil {
			return nil, err
		}
		return inner, nil
	}
	return nil, p.errorHere("expected a value, a column or %q, found %s", "(", tok.Describe())
}
