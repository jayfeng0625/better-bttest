// SPDX-License-Identifier: Apache-2.0

package sqlengine

import (
	"fmt"
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
	out, err := e.analyze(sql, tables)
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
	c := &compiler{tables: tables, q: q}
	if q.root, err = c.scan(root); err != nil {
		return nil, err
	}
	return q, nil
}

// analyze analyzes sql with only the tables it names in the catalog. go-googlesql's SimpleCatalog matches table
// names case-insensitively, so a catalog with two tables whose names differ only in case fails every analysis.
// analyze starts from a catalog with no tables. On each "Table not found" error it adds the table with exactly the
// name the error gives, and analyzes again.
func (e *env) analyze(sql string, tables []Table) (*gsql.AnalyzerOutput, error) {
	added := map[string]bool{}
	for {
		out, err := gsql.AnalyzeStatement(sql, e.opts, e.cat, e.tf)
		if err == nil {
			return out, nil
		}
		name, ok := missingTable(err.Error())
		ti := slices.IndexFunc(tables, func(t Table) bool { return t.Name == name })
		if !ok || ti < 0 || added[name] {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if err := e.addTable(tables[ti]); err != nil {
			return nil, internal(err)
		}
		added[name] = true
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
	tables []Table
	q      *Query
}

// scanNames name the scans the engine does not support, by node kind.
var scanNames = map[string]string{
	"AggregateScan":      "GROUP BY or aggregate functions",
	"AnalyticScan":       "window functions",
	"ArrayScan":          "UNNEST",
	"JoinScan":           "JOIN",
	"SetOperationScan":   "set operations",
	"SingleRowScan":      "SELECT without FROM",
	"WithScan":           "WITH",
	"WithRefScan":        "WITH",
	"SampleScan":         "TABLESAMPLE",
	"TVFScan":            "table-valued functions",
	"PivotScan":          "PIVOT",
	"UnpivotScan":        "UNPIVOT",
	"RecursiveScan":      "recursive queries",
	"AssertScan":         "ASSERT",
	"BarrierScan":        "this query shape",
	"GroupRowsScan":      "GROUP_ROWS",
	"ExecuteAsRoleScan":  "this query shape",
	"MatchRecognizeScan": "MATCH_RECOGNIZE",
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
	ti := slices.IndexFunc(c.tables, func(t Table) bool { return t.Name == name })
	if ti < 0 {
		return nil, internal(fmt.Errorf("table %q is not in the catalog", name))
	}
	tbl := &c.tables[ti]
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
		v, err := e.Value()
		if err != nil {
			return nil, internal(err)
		}
		lv, err := literal(v)
		if err != nil {
			return nil, err
		}
		return func(*execCtx, []Value) (Value, error) { return lv, nil }, nil
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
	for _, a := range args {
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
	}
	if f := function(name, xs, ts); f != nil {
		return f, nil
	}
	sqlName, err := fn.SQLName()
	if err != nil {
		return nil, internal(err)
	}
	return nil, unsupported(sqlName)
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
	switch k {
	case gsql.TypeKindTypeBytes:
		s, err := v.BytesValue()
		return Value{Bytes: []byte(s)}, err
	case gsql.TypeKindTypeString:
		s, err := v.StringValue()
		return Value{Bytes: []byte(s)}, err
	case gsql.TypeKindTypeInt64:
		i, err := v.Int64Value()
		return Value{Int: i}, err
	case gsql.TypeKindTypeBool:
		b, err := v.BoolValue()
		return Value{Bool: b}, err
	}
	t, err := v.Type()
	if err != nil {
		return Value{}, internal(err)
	}
	_, err = engineType(t)
	if err == nil {
		err = unsupported("this literal")
	}
	return Value{}, err
}
