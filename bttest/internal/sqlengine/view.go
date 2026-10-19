// SPDX-License-Identifier: Apache-2.0

//go:build googlesql

package sqlengine

import (
	"slices"

	gsql "github.com/goccy/go-googlesql"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// viewShape is what the compiler records of a view's query: its GROUP BY and ORDER BY clauses, and the columns that
// carry the source table's _key unmodified.
type viewShape struct {
	group  viewClause
	order  viewClause
	keyIDs map[int32]bool
}

// viewClause records a view's GROUP BY or ORDER BY clauses: how many the query has, and the key parts of the
// outermost one, which compiles last.
type viewClause struct {
	count int
	keys  []keyPart
}

func (vc *viewClause) add(ids []int32, kinds []Kind) {
	if len(ids) == 0 {
		return
	}
	vc.count++
	vc.keys = make([]keyPart, len(ids))
	for i, id := range ids {
		vc.keys[i] = keyPart{id: id, kind: kinds[i]}
	}
}

// unstable names the aggregates production rejects in a view as not stable, and volatile the scalar functions it
// rejects as not immutable.
var (
	unstable = map[string]bool{"any_value": true, "array_agg": true, "string_agg": true}
	volatile = map[string]bool{"current_timestamp": true, "rand": true, "generate_uuid": true, "current_date": true}
)

// viewFunctionRule fails a view's computed column that calls a volatile function before the engine checks the
// column's type, as production does.
func viewFunctionRule(cc *gsql.ResolvedComputedColumn) error {
	e, err := cc.Expr()
	if err != nil {
		return internal(err)
	}
	call, ok := e.(*gsql.ResolvedFunctionCall)
	if !ok {
		return nil
	}
	fn, err := call.Function()
	if err != nil {
		return internal(err)
	}
	name, err := fn.Name()
	if err != nil {
		return internal(err)
	}
	if volatile[name] {
		return status.Errorf(codes.InvalidArgument, "Only immutable functions are supported in materialized views (GoogleSQL:%s is not immutable)", name)
	}
	return nil
}

// CheckView analyzes and compiles a materialized view's query against the tables, and checks it against
// production's view rules with production's messages. A view reads tables only, so a view over a view fails as a
// missing table.
func CheckView(sql string, tables []Table) (*View, error) {
	p, err := compileView(sql, tables)
	if err != nil {
		return nil, err
	}
	v := &View{sql: sql}
	v.plan.Store(p)
	return v, nil
}

func compileView(sql string, tables []Table) (*viewPlan, error) {
	var sources []Table
	for _, t := range tables {
		if t.View == nil {
			sources = append(sources, t)
		}
	}
	c := &compiler{view: true, shape: viewShape{keyIDs: map[int32]bool{}}}
	q, err := prepare(sql, sources, nil, c)
	if err != nil {
		return nil, err
	}
	if c.shape.group.count > 0 && c.shape.order.count > 0 {
		return nil, status.Error(codes.InvalidArgument, "ORDER BY is not supported with GROUP BY in materialized views.")
	}
	if c.shape.group.count > 1 || c.shape.order.count > 1 {
		return nil, status.Error(codes.InvalidArgument, "This query is not valid. Please ensure that all parts of the query are valid, as per the requirements listed at https://cloud.google.com/bigtable/docs/reference/sql/googlesql-reference-overview. In particular, the query must not use multiple GROUP BY or ORDER BY clauses.")
	}
	keys, clause := c.shape.group.keys, "GROUP BY"
	if keys == nil {
		keys, clause = c.shape.order.keys, "ORDER BY"
	}
	if keys == nil {
		return nil, status.Error(codes.InvalidArgument, "queries must contain a GROUP BY or ORDER BY clause")
	}
	for i, k := range keys {
		j := slices.Index(q.outIDs, k.id)
		if j < 0 {
			return nil, status.Errorf(codes.InvalidArgument, "every %s column must be selected in the final query", clause)
		}
		keys[i].raw = len(keys) == 1 && q.Columns[j].Name == "_key"
	}
	isKey := func(k keyPart) bool { return c.shape.keyIDs[k.id] }
	if clause == "ORDER BY" && !slices.ContainsFunc(keys, isKey) {
		return nil, status.Error(codes.InvalidArgument, "queries must select and order by the unmodified _key column from the source table")
	}
	hint := slices.ContainsFunc(q.Columns, func(c Column) bool { return c.Name == "_key" })
	if clause == "GROUP BY" && hint && !(len(keys) == 1 && isKey(keys[0])) {
		return nil, status.Error(codes.InvalidArgument, "queries that provide a _key hint must only group by _key (and optionally _timestamp). Use a different column name if you want to create a composite key.")
	}
	for _, k := range keys {
		switch k.kind {
		case KindBytes, KindString, KindInt64:
		default:
			return nil, unsupported("a view key that is not BYTES, STRING or INT64")
		}
	}
	q.keys = keys
	loneKey := len(keys) == 1 && keys[0].raw
	q.cells = make([]bool, len(q.outIDs))
	for i, id := range q.outIDs {
		inKey := slices.ContainsFunc(keys, func(k keyPart) bool { return k.id == id })
		q.cells[i] = !inKey || clause == "ORDER BY" && !loneKey
	}
	return &viewPlan{query: q, families: c.tables[q.Table].Families}, nil
}

// CurrentView returns the view's compiled query while its source table's families are as they were when the query
// compiled. Once they differ, it compiles the query again and keeps the result, so the view reads, or fails, as its
// query does against the table now.
func CurrentView(v *View, tables []Table) (*Query, error) {
	p := v.plan.Load()
	i := slices.IndexFunc(tables, func(t Table) bool { return t.View == nil && t.Name == p.query.Table })
	if i >= 0 && slices.Equal(p.families, tables[i].Families) {
		return p.query, nil
	}
	p, err := compileView(v.sql, tables)
	if err != nil {
		return nil, err
	}
	v.plan.Store(p)
	return p.query, nil
}

func (e *env) addView(t Table, tables []Table) error {
	v, err := CurrentView(t.View, tables)
	if err != nil {
		return err
	}
	if err := e.addSimpleTable(t.Name, v.Columns); err != nil {
		return internal(err)
	}
	e.views[t.Name] = v
	return nil
}

// viewScan compiles a scan of a view. A query over a view depends on the view's source table and families.
func (c *compiler) viewScan(name string, s *gsql.ResolvedTableScan, v *Query) (scan, error) {
	c.q.View = name
	c.q.Table = v.Table
	c.q.Families = append(c.q.Families, v.Families...)
	cols, err := scanColumns(s)
	if err != nil {
		return nil, err
	}
	return &viewScan{view: v, cols: cols}, nil
}
