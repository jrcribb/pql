// Package pql provides a Pipeline Query Language that can be translated into SQL.
package pql

import (
	"fmt"
	"strings"

	"github.com/runreveal/pql/parser"
)

// Compile converts the given Pipeline Query Language statement
// into the equivalent SQL.
func Compile(source string) (string, error) {
	expr, err := parser.Parse(source)
	if err != nil {
		return "", err
	}

	subqueries, err := splitQueries(expr)
	if err != nil {
		return "", err
	}

	sb := new(strings.Builder)
	ctes := subqueries[:len(subqueries)-1]
	query := subqueries[len(subqueries)-1]
	if len(ctes) > 0 {
		sb.WriteString("WITH ")
		for i, sub := range ctes {
			quoteIdentifier(sb, sub.name)
			sb.WriteString(" AS (")
			if err := sub.write(sb, source); err != nil {
				return "", err
			}
			sb.WriteString(")")
			if i < len(ctes)-1 {
				sb.WriteString(",")
			}
			sb.WriteString("\n")
		}
	}
	if err := query.write(sb, source); err != nil {
		return "", err
	}
	sb.WriteString(";")
	return sb.String(), nil
}

type subquery struct {
	name      string
	sourceSQL string

	op   parser.TabularOperator
	sort *parser.SortOperator
	take *parser.TakeOperator
}

func splitQueries(expr *parser.TabularExpr) ([]*subquery, error) {
	var subqueries []*subquery
	var lastSubquery *subquery
	for i := 0; i < len(expr.Operators); i++ {
		switch op := expr.Operators[i].(type) {
		case *parser.SortOperator:
			if lastSubquery != nil && canAttachSort(lastSubquery.op) && lastSubquery.sort == nil && lastSubquery.take == nil {
				lastSubquery.sort = op
			} else {
				lastSubquery = &subquery{
					sort: op,
				}
				subqueries = append(subqueries, lastSubquery)
			}
		case *parser.TakeOperator:
			if lastSubquery != nil && canAttachSort(lastSubquery.op) && lastSubquery.take == nil {
				lastSubquery.take = op
			} else {
				lastSubquery = &subquery{
					take: op,
				}
				subqueries = append(subqueries, lastSubquery)
			}
		default:
			lastSubquery = &subquery{
				op: op,
			}
			subqueries = append(subqueries, lastSubquery)
		}
	}

	if len(subqueries) == 0 {
		subqueries = append(subqueries, new(subquery))
	}
	buf := new(strings.Builder)
	for i, sub := range subqueries {
		if i == 0 {
			var err error
			sub.sourceSQL, err = dataSourceSQL(expr.Source)
			if err != nil {
				return nil, err
			}
		} else {
			buf.Reset()
			quoteIdentifier(buf, subqueries[i-1].name)
			sub.sourceSQL = buf.String()
		}

		if i < len(subqueries)-1 {
			sub.name = fmt.Sprintf("subquery%d", i)
		}
	}

	return subqueries, nil
}

// canAttachSort reports whether the given operator's subquery can have a sort clause attached.
// This becomes significant for operators like "project"
// because they change the identifiers in scope.
func canAttachSort(op parser.TabularOperator) bool {
	switch op.(type) {
	case *parser.ProjectOperator, *parser.SummarizeOperator:
		return false
	default:
		return true
	}
}

func (sub *subquery) write(sb *strings.Builder, source string) error {
	switch op := sub.op.(type) {
	case nil:
		sb.WriteString("SELECT * FROM ")
		sb.WriteString(sub.sourceSQL)
	case *parser.ProjectOperator:
		sb.WriteString("SELECT ")
		for i, col := range op.Cols {
			if i > 0 {
				sb.WriteString(", ")
			}
			if col.X == nil {
				if err := writeExpression(sb, source, col.Name); err != nil {
					return err
				}
			} else {
				if err := writeExpression(sb, source, col.X); err != nil {
					return err
				}
			}
			sb.WriteString(" AS ")
			quoteIdentifier(sb, col.Name.Name)
		}
		sb.WriteString(" FROM ")
		sb.WriteString(sub.sourceSQL)
	case *parser.WhereOperator:
		sb.WriteString("SELECT * FROM ")
		sb.WriteString(sub.sourceSQL)
		sb.WriteString(" WHERE ")
		if err := writeExpression(sb, source, op.Predicate); err != nil {
			return err
		}
	case *parser.CountOperator:
		sb.WriteString("SELECT COUNT(*) FROM ")
		sb.WriteString(sub.sourceSQL)
	default:
		fmt.Fprintf(sb, "SELECT NULL /* unsupported operator %T */", op)
		return nil
	}

	if sub.sort != nil {
		sb.WriteString(" ORDER BY ")
		for i, term := range sub.sort.Terms {
			if err := writeExpression(sb, source, term.X); err != nil {
				return err
			}
			if term.Asc {
				sb.WriteString(" ASC")
			} else {
				sb.WriteString(" DESC")
			}
			if term.NullsFirst {
				sb.WriteString(" NULLS FIRST")
			} else {
				sb.WriteString(" NULLS LAST")
			}
			if i < len(sub.sort.Terms)-1 {
				sb.WriteString(", ")
			}
		}
	}

	if sub.take != nil {
		sb.WriteString(" LIMIT ")
		if err := writeExpression(sb, source, sub.take.RowCount); err != nil {
			return err
		}
	}

	return nil
}

func dataSourceSQL(src parser.TabularDataSource) (string, error) {
	switch src := src.(type) {
	case *parser.TableRef:
		sb := new(strings.Builder)
		quoteIdentifier(sb, src.Table.Name)
		return sb.String(), nil
	default:
		return "", fmt.Errorf("unhandled data source %T", src)
	}
}

func quoteIdentifier(sb *strings.Builder, name string) {
	const quoteEscape = `""`
	sb.Grow(len(name) + strings.Count(name, `"`)*(len(quoteEscape)-1) + len(`""`))

	sb.WriteString(`"`)
	for _, b := range []byte(name) {
		if b == '"' {
			sb.WriteString(quoteEscape)
		} else {
			sb.WriteByte(b)
		}
	}
	sb.WriteString(`"`)
}

var binaryOps = map[parser.TokenKind]string{
	parser.TokenAnd:   "AND",
	parser.TokenOr:    "OR",
	parser.TokenPlus:  "+",
	parser.TokenMinus: "-",
	parser.TokenStar:  "*",
	parser.TokenSlash: "/",
	parser.TokenMod:   "%",
	parser.TokenLT:    "<",
	parser.TokenLE:    "<=",
	parser.TokenGT:    ">",
	parser.TokenGE:    ">=",
}

func writeExpression(sb *strings.Builder, source string, x parser.Expr) error {
	// Unwrap any parentheses.
	// We manually insert parentheses as needed.
	for {
		p, ok := x.(*parser.ParenExpr)
		if !ok {
			break
		}
		x = p
	}

	switch x := x.(type) {
	case *parser.Ident:
		quoteIdentifier(sb, x.Name)
	case *parser.BasicLit:
		switch x.Kind {
		case parser.TokenNumber:
			sb.WriteString(x.Value)
		case parser.TokenString:
			quoteSQLString(sb, x.Value)
		default:
			fmt.Fprintf(sb, "NULL /* unhandled %s literal */", x.Kind)
		}
	case *parser.UnaryExpr:
		switch x.Op {
		case parser.TokenPlus:
			sb.WriteString("+")
		case parser.TokenMinus:
			sb.WriteString("-")
		default:
			fmt.Fprintf(sb, "/* unhandled %s unary op */ ", x.Op)
		}
		if err := writeExpression(sb, source, x.X); err != nil {
			return err
		}
	case *parser.BinaryExpr:
		switch x.Op {
		case parser.TokenEq:
			sb.WriteString("coalesce((")
			if err := writeExpression(sb, source, x.X); err != nil {
				return err
			}
			sb.WriteString(") = (")
			if err := writeExpression(sb, source, x.Y); err != nil {
				return err
			}
			sb.WriteString("), FALSE)")
		case parser.TokenNE:
			sb.WriteString("coalesce((")
			if err := writeExpression(sb, source, x.X); err != nil {
				return err
			}
			sb.WriteString(") <> (")
			if err := writeExpression(sb, source, x.Y); err != nil {
				return err
			}
			sb.WriteString("), FALSE)")
		case parser.TokenCaseInsensitiveEq:
			sb.WriteString("lower(")
			if err := writeExpression(sb, source, x.X); err != nil {
				return err
			}
			sb.WriteString(") = lower(")
			if err := writeExpression(sb, source, x.Y); err != nil {
				return err
			}
			sb.WriteString(")")
		case parser.TokenCaseInsensitiveNE:
			sb.WriteString("lower(")
			if err := writeExpression(sb, source, x.X); err != nil {
				return err
			}
			sb.WriteString(") <> lower(")
			if err := writeExpression(sb, source, x.Y); err != nil {
				return err
			}
			sb.WriteString(")")
		default:
			if sqlOp, ok := binaryOps[x.Op]; ok {
				sb.WriteString("(")
				if err := writeExpression(sb, source, x.X); err != nil {
					return err
				}
				sb.WriteString(") ")
				sb.WriteString(sqlOp)
				sb.WriteString(" (")
				if err := writeExpression(sb, source, x.Y); err != nil {
					return err
				}
				sb.WriteString(")")
			} else {
				fmt.Fprintf(sb, "NULL /* unhandled %s binary op */ ", x.Op)
			}
		}
	case *parser.CallExpr:
		switch x.Func.Name {
		case "not":
			if len(x.Args) != 1 {
				return &compileError{
					source: source,
					span: parser.Span{
						Start: x.Lparen.End,
						End:   x.Rparen.Start,
					},
					err: fmt.Errorf("not(x) takes a single argument (got %d)", len(x.Args)),
				}
			}
			sb.WriteString("NOT (")
			if err := writeExpression(sb, source, x.Args[0]); err != nil {
				return err
			}
			sb.WriteString(")")
		default:
			sb.WriteString(x.Func.Name)
			sb.WriteString("(")
			for i, arg := range x.Args {
				if i > 0 {
					sb.WriteString(", ")
				}
				if err := writeExpression(sb, source, arg); err != nil {
					return err
				}
			}
			sb.WriteString(")")
		}
	default:
		fmt.Fprintf(sb, "NULL /* unhandled %T expression */", x)
	}
	return nil
}

func quoteSQLString(sb *strings.Builder, s string) {
	sb.WriteString("'")
	for _, b := range []byte(s) {
		if b == '\'' {
			sb.WriteString("''")
		} else {
			sb.WriteByte(b)
		}
	}
	sb.WriteString("'")
}

type compileError struct {
	source string
	span   parser.Span
	err    error
}

func (e *compileError) Error() string {
	line, col := linecol(e.source, e.span.Start)
	return fmt.Sprintf("%d:%d: %s", line, col, e.err.Error())
}

func (e *compileError) Unwrap() error {
	return e.err
}

func linecol(source string, pos int) (line, col int) {
	line, col = 1, 1
	for _, c := range source[:pos] {
		switch c {
		case '\n':
			line++
			col = 1
		case '\t':
			const tabWidth = 8
			tabLoc := (col - 1) % tabWidth
			col += tabWidth - tabLoc
		default:
			col++
		}
	}
	return
}
