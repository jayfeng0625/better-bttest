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
	// View is the materialized view the query reads, or empty.
	View string
	// Families are the families the query reads.
	Families []string

	root   scan
	outIDs []int32
	nSlots int
	// keys are a materialized view's key parts, in key order, and cells marks each output column that a view row
	// stores as a cell. Only PrepareView sets them.
	keys  []keyPart
	cells []bool
}

// Prepare analyzes sql against the tables and compiles it. It returns InvalidArgument with the analyzer's one-line
// message, as production does, and InvalidArgument naming a construct the engine does not support.
func Prepare(sql string, tables []Table, params map[string]Type) (*Query, error) {
	return prepare(sql, tables, params, &compiler{})
}

// prepare analyzes and compiles sql with the compiler c, which records a view's shape.
func prepare(sql string, tables []Table, params map[string]Type, c *compiler) (*Query, error) {
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
	// A view checks its functions before its output types, as production does, so an output type the engine does
	// not support fails the view only after its query compiles.
	var typeErr error
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
			if !c.view {
				return nil, err
			}
			if typeErr == nil {
				typeErr = err
			}
		}
		q.Columns = append(q.Columns, Column{Name: name, Type: t})
		q.outIDs = append(q.outIDs, id)
	}
	root, err := qs.Query()
	if err != nil {
		return nil, internal(err)
	}
	c.tables, c.views, c.q = byName, e.views, q
	if q.root, err = c.scan(root); err != nil {
		return nil, err
	}
	if typeErr != nil {
		return nil, typeErr
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
		if tables[ti].ViewQuery != "" {
			if err := e.addView(tables[ti], tables); err != nil {
				return nil, nil, err
			}
		} else if err := e.addTable(tables[ti]); err != nil {
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
	views  map[string]*Query
	q      *Query
	// view marks a materialized view's query. The compiler then records the view's shape.
	view  bool
	shape viewShape
}

// scanNames name the scans the engine does not support, by node kind.
var scanNames = map[string]string{
	"JoinScan":         "JOIN",
	"SetOperationScan": "set operations",
	"SingleRowScan":    "SELECT without FROM",
	"WithScan":         "WITH",
}

// subqueryNames name the subqueries the analyzer accepts, by type, other than a scalar subquery. A scalar subquery
// fails with production's message.
var subqueryNames = map[gsql.ResolvedSubqueryExprEnums_SubqueryType]string{
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
		if p.ids, _, p.exprs, err = c.computed(cols); err != nil {
			return nil, err
		}
		return p, nil
	case *gsql.ResolvedOrderByScan:
		return c.orderBy(s)
	case *gsql.ResolvedAggregateScan:
		return c.aggregate(s)
	case *gsql.ResolvedArrayScan:
		return c.arrayScan(s)
	case *gsql.ResolvedLimitOffsetScan:
		if c.view {
			return nil, status.Error(codes.InvalidArgument, "Limit and offset are not supported in materialized views.")
		}
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

// distinctError rejects the aggregate scan of SELECT DISTINCT. The analyzer puts the output columns of SELECT DISTINCT
// in the $distinct table.
func distinctError(s *gsql.ResolvedAggregateScan) error {
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
	return nil
}

func (c *compiler) input(n gsql.ResolvedScanNode, err error) (scan, error) {
	if err != nil {
		return nil, internal(err)
	}
	return c.scan(n)
}

func (c *compiler) computed(cols []*gsql.ResolvedComputedColumn) ([]int32, []Kind, []expr, error) {
	var ids []int32
	var kinds []Kind
	var exprs []expr
	for _, cc := range cols {
		if c.view {
			if err := viewFunctionRule(cc); err != nil {
				return nil, nil, nil, err
			}
		}
		id, t, e, err := computedColumn(cc)
		if err != nil {
			return nil, nil, nil, err
		}
		if ref, ok := e.(*gsql.ResolvedColumnRef); ok && c.view {
			col, err := ref.Column()
			if err != nil {
				return nil, nil, nil, internal(err)
			}
			refID, _, err := colInfo(col)
			if err != nil {
				return nil, nil, nil, err
			}
			if c.shape.keyIDs[refID] {
				c.markKey(id)
			}
		}
		x, err := c.expr(e)
		if err != nil {
			return nil, nil, nil, err
		}
		ids = append(ids, id)
		kinds = append(kinds, t.Kind)
		exprs = append(exprs, x)
	}
	return ids, kinds, exprs, nil
}

// markKey records that a view's column id carries the source table's _key unmodified: the table's own _key column,
// or a column computed as a plain reference to one.
func (c *compiler) markKey(id int32) {
	if c.shape.keyIDs == nil {
		c.shape.keyIDs = map[int32]bool{}
	}
	c.shape.keyIDs[id] = true
}

// setViewKeys records a view's GROUP BY or ORDER BY clause and its key parts.
func (c *compiler) setViewKeys(dst *viewClause, ids []int32, kinds []Kind) {
	if !c.view || len(ids) == 0 {
		return
	}
	dst.add(ids, kinds)
}

// arrayScan compiles a comma join or CROSS JOIN with UNNEST of one array.
func (c *compiler) arrayScan(s *gsql.ResolvedArrayScan) (scan, error) {
	in, err := s.InputScan()
	if err != nil {
		return nil, internal(err)
	}
	if in == nil {
		return nil, unsupported("UNNEST without a table")
	}
	if outer, err := s.IsOuter(); err != nil {
		return nil, internal(err)
	} else if outer {
		return nil, unsupported("LEFT JOIN")
	}
	if je, err := s.JoinExpr(); err != nil {
		return nil, internal(err)
	} else if je != nil {
		return nil, unsupported("JOIN with ON")
	}
	if oc, err := s.ArrayOffsetColumn(); err != nil {
		return nil, internal(err)
	} else if oc != nil {
		return nil, unsupported("WITH OFFSET")
	}
	exprs, err := s.ArrayExprList()
	if err != nil {
		return nil, internal(err)
	}
	cols, err := s.ElementColumnList()
	if err != nil {
		return nil, internal(err)
	}
	if len(exprs) != 1 || len(cols) != 1 {
		return nil, unsupported("UNNEST of more than one array")
	}
	input, err := c.scan(in)
	if err != nil {
		return nil, err
	}
	a := &arrayScan{input: input}
	if a.array, err = c.expr(exprs[0]); err != nil {
		return nil, err
	}
	if a.elemID, _, err = colInfo(cols[0]); err != nil {
		return nil, err
	}
	return a, nil
}

func (c *compiler) aggregate(s *gsql.ResolvedAggregateScan) (scan, error) {
	if err := distinctError(s); err != nil {
		return nil, err
	}
	if sets, err := s.GroupingSetList(); err != nil {
		return nil, internal(err)
	} else if len(sets) > 0 {
		return nil, unsupported("GROUPING SETS, ROLLUP or CUBE")
	}
	input, err := c.input(s.InputScan())
	if err != nil {
		return nil, err
	}
	a := &aggregate{input: input}
	gb, err := s.GroupByList()
	if err != nil {
		return nil, internal(err)
	}
	var kinds []Kind
	if a.groupIDs, kinds, a.groupExprs, err = c.computed(gb); err != nil {
		return nil, err
	}
	c.setViewKeys(&c.shape.group, a.groupIDs, kinds)
	al, err := s.AggregateList()
	if err != nil {
		return nil, internal(err)
	}
	for _, cb := range al {
		cc, ok := cb.(*gsql.ResolvedComputedColumn)
		if !ok {
			return nil, unsupported("this aggregate")
		}
		ag, err := c.aggregateCall(cc)
		if err != nil {
			return nil, err
		}
		a.aggs = append(a.aggs, ag)
	}
	return a, nil
}

func computedColumn(cc *gsql.ResolvedComputedColumn) (int32, Type, gsql.ResolvedExprNode, error) {
	col, err := cc.Column()
	if err != nil {
		return 0, Type{}, nil, internal(err)
	}
	id, t, err := colInfo(col)
	if err != nil {
		return 0, Type{}, nil, err
	}
	e, err := cc.Expr()
	if err != nil {
		return 0, Type{}, nil, internal(err)
	}
	return id, t, e, nil
}

func (c *compiler) aggregateCall(cc *gsql.ResolvedComputedColumn) (aggSpec, error) {
	id, t, e, err := computedColumn(cc)
	if err != nil {
		return aggSpec{}, err
	}
	call, ok := e.(*gsql.ResolvedAggregateFunctionCall)
	if !ok {
		return aggSpec{}, unsupported("this aggregate")
	}
	fn, err := call.Function()
	if err != nil {
		return aggSpec{}, internal(err)
	}
	name, err := fn.Name()
	if err != nil {
		return aggSpec{}, internal(err)
	}
	sqlName, err := fn.SQLName()
	if err != nil {
		return aggSpec{}, internal(err)
	}
	if d, err := call.Distinct(); err != nil {
		return aggSpec{}, internal(err)
	} else if d {
		return aggSpec{}, unsupported(sqlName + " with DISTINCT")
	}
	if hm, err := call.HavingModifier(); err != nil {
		return aggSpec{}, internal(err)
	} else if hm != nil {
		return aggSpec{}, unsupported(sqlName + " with HAVING MAX or HAVING MIN")
	}
	args, err := call.ArgumentList()
	if err != nil {
		return aggSpec{}, internal(err)
	}
	if c.view && unstable[name] {
		return aggSpec{}, status.Errorf(codes.InvalidArgument, "Only stable functions are supported in materialized views (GoogleSQL:%s is not stable)", name)
	}
	ag := aggSpec{id: id, kind: t.Kind}
	switch {
	case name == "$count_star":
		ag.fn = aggCountStar
	case name == "sum" && t.Kind == KindInt64:
		ag.fn = aggSum
	case name == "max" && t.Kind != KindArray && t.Kind != KindMap:
		ag.fn = aggMax
	default:
		return aggSpec{}, unsupported(sqlName)
	}
	if len(args) > 0 {
		if ag.arg, err = c.expr(args[0]); err != nil {
			return aggSpec{}, err
		}
	}
	return ag, nil
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
	var ids []int32
	var kinds []Kind
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
		if desc && c.view {
			return nil, status.Error(codes.InvalidArgument, "Only ascending order in ORDER BY by is supported in materialized views.")
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
		ids = append(ids, id)
		kinds = append(kinds, t.Kind)
	}
	c.setViewKeys(&c.shape.order, ids, kinds)
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
	if v := c.views[name]; v != nil {
		return c.viewScan(name, s, v)
	}
	tbl := c.tables[name]
	c.q.Table = name
	ts := &tableScan{table: name}
	cols, err := scanColumns(s)
	if err != nil {
		return nil, err
	}
	for _, sc := range cols {
		tc := tableCol{id: sc.id}
		if sc.index == 0 && c.view {
			c.markKey(sc.id)
		}
		if sc.index > 0 {
			tc.family = &tbl.Families[sc.index-1]
			c.q.Families = append(c.q.Families, tc.family.Name)
		}
		ts.cols = append(ts.cols, tc)
	}
	return ts, nil
}

// scanCol is one column a table scan reads: the slot it fills and the column's index in the table.
type scanCol struct {
	id    int32
	index int
}

func scanColumns(s *gsql.ResolvedTableScan) ([]scanCol, error) {
	idx, err := s.ColumnIndexList()
	if err != nil {
		return nil, internal(err)
	}
	cols := make([]scanCol, len(idx))
	for i, index := range idx {
		col, err := s.ColumnList2(int32(i))
		if err != nil {
			return nil, internal(err)
		}
		if cols[i].id, _, err = colInfo(col); err != nil {
			return nil, err
		}
		cols[i].index = int(index)
	}
	return cols, nil
}

func (c *compiler) expr(n gsql.ResolvedExprNode) (expr, error) {
	switch e := n.(type) {
	case *gsql.ResolvedLiteral:
		v, err := literal(e)
		if err != nil {
			return nil, err
		}
		return constExpr(v), nil
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
		if st == gsql.ResolvedSubqueryExprEnums_SubqueryTypeScalar {
			return nil, status.Error(codes.InvalidArgument, "Subqueries are not supported")
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
		var x expr
		if l, ok := a.(*gsql.ResolvedLiteral); ok {
			v, err := literal(l)
			if err != nil {
				return nil, err
			}
			consts[i] = &v
			x = constExpr(v)
		} else if x, err = c.expr(a); err != nil {
			return nil, err
		}
		t, err := exprType(a)
		if err != nil {
			return nil, err
		}
		xs = append(xs, x)
		ts = append(ts, t)
	}
	if name == "json_query_array" && (consts[1] == nil || string(consts[1].Bytes) != "$") {
		return nil, unsupported("JSON_QUERY_ARRAY with a JSONPath other than $")
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

func constExpr(v Value) expr {
	return func(*execCtx, []Value) (Value, error) { return v, nil }
}

func literal(l *gsql.ResolvedLiteral) (Value, error) {
	v, err := l.Value()
	if err != nil {
		return Value{}, internal(err)
	}
	return literalValue(v)
}

func literalValue(v *gsql.Value) (Value, error) {
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
	case gsql.TypeKindTypeArray:
		els, err := v.Elements()
		if err != nil {
			return Value{}, internal(err)
		}
		out.Elems = make([]Value, len(els))
		for i, el := range els {
			if out.Elems[i], err = literalValue(el); err != nil {
				return Value{}, err
			}
		}
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
