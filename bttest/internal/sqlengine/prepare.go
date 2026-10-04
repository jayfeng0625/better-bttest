// SPDX-License-Identifier: Apache-2.0

package sqlengine

import (
	"slices"
	"strings"

	gsql "github.com/goccy/go-googlesql"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Column is one output column of a prepared query.
type Column struct {
	Name string
	Type Type
}

// Query is a prepared query. It holds no go-googlesql handle, so it is safe for concurrent use.
type Query struct {
	// Columns are the output columns, in order.
	Columns []Column
	// Table is the table the query reads.
	Table string
	// Families are the families the query reads.
	Families []string

	root   scan
	outIDs []int32
	nSlots int
}

// Prepare analyzes sql against the tables and compiles it. It returns InvalidArgument with the analyzer's one-line
// message, as production does, and InvalidArgument naming a construct the engine does not support.
func Prepare(sql string, tables []Table, params map[string]Type) (*Query, error) {
	e, err := newEnv(params)
	if err != nil {
		return nil, internal(err)
	}
	out, byName, err := e.analyze(sql, tables)
	if err != nil {
		return nil, err
	}
	st, err := out.ResolvedStatement()
	if err != nil {
		return nil, internal(err)
	}
	qs, ok := st.(*gsql.ResolvedQueryStmt)
	if !ok {
		return nil, unsupported("statements other than SELECT")
	}
	maxID, err := out.MaxColumnId()
	if err != nil {
		return nil, internal(err)
	}
	q := &Query{nSlots: int(maxID) + 1}
	n, err := qs.OutputColumnListSize()
	if err != nil {
		return nil, internal(err)
	}
	for i := range n {
		oc, err := qs.OutputColumnList2(i)
		if err != nil {
			return nil, internal(err)
		}
		name, err := oc.Name()
		if err != nil {
			return nil, internal(err)
		}
		col, err := oc.Column()
		if err != nil {
			return nil, internal(err)
		}
		id, t, err := colInfo(col)
		if err != nil {
			return nil, err
		}
		q.Columns = append(q.Columns, Column{Name: name, Type: t})
		q.outIDs = append(q.outIDs, id)
	}
	root, err := qs.Query()
	if err != nil {
		return nil, internal(err)
	}
	c := &compiler{tables: byName, q: q}
	if q.root, err = c.scan(root); err != nil {
		return nil, err
	}
	return q, nil
}

// analyze analyzes sql with only the tables it names in the catalog. go-googlesql's SimpleCatalog matches table
// names case-insensitively, so a catalog with two tables whose names differ only in case fails every analysis.
// analyze starts from a catalog with no tables. On each "Table not found" error it adds the table with exactly the
// name the error gives, and analyzes again. It returns the tables it added, by name.
func (e *env) analyze(sql string, tables []Table) (*gsql.AnalyzerOutput, map[string]*Table, error) {
	added := map[string]*Table{}
	for {
		out, err := gsql.AnalyzeStatement(sql, e.opts, e.cat, e.tf)
		if err == nil {
			return out, added, nil
		}
		name, ok := missingTable(err.Error())
		ti := slices.IndexFunc(tables, func(t Table) bool { return t.Name == name })
		if !ok || ti < 0 || added[name] != nil {
			return nil, nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if err := e.addTable(tables[ti]); err != nil {
			return nil, nil, internal(err)
		}
		added[name] = &tables[ti]
	}
}

// missingTable returns the table name in the analyzer's "Table not found: <name> [at <line>:<column>]" message.
func missingTable(msg string) (string, bool) {
	rest, ok := strings.CutPrefix(msg, "Table not found: ")
	if !ok {
		return "", false
	}
	name, _, ok := strings.Cut(rest, " [at ")
	if !ok {
		return "", false
	}
	return strings.Trim(name, "`"), true
}

func internal(err error) error { return status.Errorf(codes.Internal, "sql engine: %v", err) }

func unsupported(what string) error {
	return status.Errorf(codes.InvalidArgument, "The emulator does not support %s", what)
}

func colInfo(c *gsql.ResolvedColumn) (int32, Type, error) {
	id, err := c.ColumnId()
	if err != nil {
		return 0, Type{}, internal(err)
	}
	gt, err := c.Type()
	if err != nil {
		return 0, Type{}, internal(err)
	}
	t, err := engineType(gt)
	return id, t, err
}

func exprType(n gsql.ResolvedExprNode) (Type, error) {
	gt, err := n.Type()
	if err != nil {
		return Type{}, internal(err)
	}
	return engineType(gt)
}

type compiler struct {
	tables map[string]*Table
	q      *Query
}

// scanNames name the scans the engine does not support, by node kind. The analyzer rejects the other scans.
var scanNames = map[string]string{
	"ArrayScan":        "UNNEST",
	"JoinScan":         "JOIN",
	"SetOperationScan": "set operations",
	"SingleRowScan":    "SELECT without FROM",
	"WithScan":         "WITH",
}

// subqueryNames name the subqueries the analyzer accepts, by type.
var subqueryNames = map[gsql.ResolvedSubqueryExprEnums_SubqueryType]string{
	gsql.ResolvedSubqueryExprEnums_SubqueryTypeScalar: "scalar subqueries",
	gsql.ResolvedSubqueryExprEnums_SubqueryTypeArray:  "ARRAY subqueries",
	gsql.ResolvedSubqueryExprEnums_SubqueryTypeExists: "EXISTS subqueries",
	gsql.ResolvedSubqueryExprEnums_SubqueryTypeIn:     "IN subqueries",
}

func (c *compiler) scan(n gsql.ResolvedScanNode) (scan, error) {
	switch s := n.(type) {
	case *gsql.ResolvedTableScan:
		return c.tableScan(s)
	case *gsql.ResolvedFilterScan:
		input, err := c.input(s.InputScan())
		if err != nil {
			return nil, err
		}
		fe, err := s.FilterExpr()
		if err != nil {
			return nil, internal(err)
		}
		pred, err := c.expr(fe)
		if err != nil {
			return nil, err
		}
		return &filter{input: input, pred: pred}, nil
	case *gsql.ResolvedProjectScan:
		input, err := c.input(s.InputScan())
		if err != nil {
			return nil, err
		}
		p := &project{input: input}
		cols, err := s.ExprList()
		if err != nil {
			return nil, internal(err)
		}
		for _, cc := range cols {
			col, err := cc.Column()
			if err != nil {
				return nil, internal(err)
			}
			id, _, err := colInfo(col)
			if err != nil {
				return nil, err
			}
			e, err := cc.Expr()
			if err != nil {
				return nil, internal(err)
			}
			x, err := c.expr(e)
			if err != nil {
				return nil, err
			}
			p.ids = append(p.ids, id)
			p.exprs = append(p.exprs, x)
		}
		return p, nil
	case *gsql.ResolvedOrderByScan:
		return c.orderBy(s)
	case *gsql.ResolvedLimitOffsetScan:
		input, err := c.input(s.InputScan())
		if err != nil {
			return nil, err
		}
		if oe, err := s.Offset(); err != nil {
			return nil, internal(err)
		} else if oe != nil {
			return nil, unsupported("OFFSET")
		}
		le, err := s.Limit()
		if err != nil {
			return nil, internal(err)
		}
		limit, err := c.expr(le)
		if err != nil {
			return nil, err
		}
		return &limitScan{input: input, limit: limit}, nil
	case *gsql.ResolvedAggregateScan:
		return nil, aggregateError(s)
	}
	kind, err := n.NodeKindString()
	if err != nil {
		return nil, internal(err)
	}
	if name, ok := scanNames[kind]; ok {
		return nil, unsupported(name)
	}
	return nil, unsupported(kind)
}

// aggregateError names the construct an aggregate scan comes from. The analyzer puts the output columns of SELECT
// DISTINCT in the $distinct table.
func aggregateError(s *gsql.ResolvedAggregateScan) error {
	cols, err := s.ColumnList()
	if err != nil {
		return internal(err)
	}
	if len(cols) > 0 {
		table, err := cols[0].TableName()
		if err != nil {
			return internal(err)
		}
		if table == "$distinct" {
			return unsupported("SELECT DISTINCT")
		}
	}
	return unsupported("GROUP BY or aggregate functions")
}

func (c *compiler) input(n gsql.ResolvedScanNode, err error) (scan, error) {
	if err != nil {
		return nil, internal(err)
	}
	return c.scan(n)
}

func (c *compiler) orderBy(s *gsql.ResolvedOrderByScan) (scan, error) {
	input, err := c.input(s.InputScan())
	if err != nil {
		return nil, err
	}
	o := &orderBy{input: input}
	items, err := s.OrderByItemList()
	if err != nil {
		return nil, internal(err)
	}
	for _, it := range items {
		ref, err := it.ColumnRef()
		if err != nil {
			return nil, internal(err)
		}
		col, err := ref.Column()
		if err != nil {
			return nil, internal(err)
		}
		id, t, err := colInfo(col)
		if err != nil {
			return nil, err
		}
		desc, err := it.IsDescending()
		if err != nil {
			return nil, internal(err)
		}
		no, err := it.NullOrder()
		if err != nil {
			return nil, internal(err)
		}
		// GoogleSQL puts NULLs first in ascending order and last in descending order.
		nullsFirst := !desc
		switch no {
		case gsql.ResolvedOrderByItemEnums_NullOrderModeNullsFirst:
			nullsFirst = true
		case gsql.ResolvedOrderByItemEnums_NullOrderModeNullsLast:
			nullsFirst = false
		}
		o.keys = append(o.keys, sortKey{id: id, kind: t.Kind, desc: desc, nullsFirst: nullsFirst})
	}
	return o, nil
}

func (c *compiler) tableScan(s *gsql.ResolvedTableScan) (scan, error) {
	tn, err := s.Table()
	if err != nil {
		return nil, internal(err)
	}
	name, err := tn.Name()
	if err != nil {
		return nil, internal(err)
	}
	tbl := c.tables[name]
	c.q.Table = name
	ts := &tableScan{table: name}
	idx, err := s.ColumnIndexList()
	if err != nil {
		return nil, internal(err)
	}
	for i, index := range idx {
		col, err := s.ColumnList2(int32(i))
		if err != nil {
			return nil, internal(err)
		}
		id, _, err := colInfo(col)
		if err != nil {
			return nil, err
		}
		tc := tableCol{id: id}
		if index > 0 {
			tc.family = &tbl.Families[index-1]
			c.q.Families = append(c.q.Families, tc.family.Name)
		}
		ts.cols = append(ts.cols, tc)
	}
	return ts, nil
}

func (c *compiler) expr(n gsql.ResolvedExprNode) (expr, error) {
	switch e := n.(type) {
	case *gsql.ResolvedLiteral:
		lv, err := constant(e)
		if err != nil {
			return nil, err
		}
		return func(*execCtx, []Value) (Value, error) { return *lv, nil }, nil
	case *gsql.ResolvedColumnRef:
		col, err := e.Column()
		if err != nil {
			return nil, internal(err)
		}
		id, _, err := colInfo(col)
		if err != nil {
			return nil, err
		}
		return func(_ *execCtx, row []Value) (Value, error) { return row[id], nil }, nil
	case *gsql.ResolvedParameter:
		name, err := e.Name()
		if err != nil {
			return nil, internal(err)
		}
		return func(x *execCtx, _ []Value) (Value, error) { return x.params[name], nil }, nil
	case *gsql.ResolvedCast:
		return c.cast(e)
	case *gsql.ResolvedFunctionCall:
		return c.call(e)
	case *gsql.ResolvedSubqueryExpr:
		st, err := e.SubqueryType()
		if err != nil {
			return nil, internal(err)
		}
		if name, ok := subqueryNames[st]; ok {
			return nil, unsupported(name)
		}
		return nil, unsupported("subqueries")
	case *gsql.ResolvedGetStructField:
		return nil, unsupported("STRUCT field access")
	}
	kind, err := n.NodeKindString()
	if err != nil {
		return nil, internal(err)
	}
	return nil, unsupported(kind)
}

func (c *compiler) cast(e *gsql.ResolvedCast) (expr, error) {
	if safe, err := e.ReturnNullOnError(); err != nil {
		return nil, internal(err)
	} else if safe {
		return nil, unsupported("SAFE_CAST")
	}
	in, err := e.Expr()
	if err != nil {
		return nil, internal(err)
	}
	from, err := exprType(in)
	if err != nil {
		return nil, err
	}
	to, err := exprType(e)
	if err != nil {
		return nil, err
	}
	x, err := c.expr(in)
	if err != nil {
		return nil, err
	}
	return castExpr(x, from, to)
}

func (c *compiler) call(e *gsql.ResolvedFunctionCall) (expr, error) {
	fn, err := e.Function()
	if err != nil {
		return nil, internal(err)
	}
	name, err := fn.Name()
	if err != nil {
		return nil, internal(err)
	}
	args, err := e.ArgumentList()
	if err != nil {
		return nil, internal(err)
	}
	var xs []expr
	var ts []Type
	consts := make([]*Value, len(args))
	for i, a := range args {
		x, err := c.expr(a)
		if err != nil {
			return nil, err
		}
		t, err := exprType(a)
		if err != nil {
			return nil, err
		}
		xs = append(xs, x)
		ts = append(ts, t)
		if l, ok := a.(*gsql.ResolvedLiteral); ok {
			if consts[i], err = constant(l); err != nil {
				return nil, err
			}
		}
	}
	if f := function(name, xs, ts, consts); f != nil {
		return f, nil
	}
	sqlName, err := fn.SQLName()
	if err != nil {
		return nil, internal(err)
	}
	return nil, unsupported(sqlName)
}

func constant(l *gsql.ResolvedLiteral) (*Value, error) {
	v, err := l.Value()
	if err != nil {
		return nil, internal(err)
	}
	lv, err := literal(v)
	if err != nil {
		return nil, err
	}
	return &lv, nil
}

func literal(v *gsql.Value) (Value, error) {
	isNull, err := v.IsNull()
	if err != nil {
		return Value{}, internal(err)
	}
	if isNull {
		return null, nil
	}
	k, err := v.TypeKind()
	if err != nil {
		return Value{}, internal(err)
	}
	var out Value
	var s string
	switch k {
	case gsql.TypeKindTypeBytes:
		s, err = v.BytesValue()
		out.Bytes = []byte(s)
	case gsql.TypeKindTypeString:
		s, err = v.StringValue()
		out.Bytes = []byte(s)
	case gsql.TypeKindTypeInt64:
		out.Int, err = v.Int64Value()
	case gsql.TypeKindTypeBool:
		out.Bool, err = v.BoolValue()
	default:
		t, err := v.Type()
		if err != nil {
			return Value{}, internal(err)
		}
		if _, err := engineType(t); err != nil {
			return Value{}, err
		}
		return Value{}, unsupported("this literal")
	}
	if err != nil {
		return Value{}, internal(err)
	}
	return out, nil
}
