// SPDX-License-Identifier: Apache-2.0

//go:build googlesql

package sqlengine

import (
	"slices"

	gsql "github.com/goccy/go-googlesql"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// unstable names the aggregates production rejects in a view as not stable, and volatile the scalar functions it
// rejects as not immutable.
var (
	unstable = map[string]bool{"any_value": true, "array_agg": true, "string_agg": true}
	volatile = map[string]bool{"current_timestamp": true, "rand": true, "generate_uuid": true, "current_date": true}
)

// viewShape is what the view rules record of a view's query: its GROUP BY and ORDER BY clauses, the source table's
// _key columns, and the computed columns that only rename another column.
type viewShape struct {
	group, order viewClause
	keyIDs       map[int32]bool
	renames      map[int32]int32
}

type viewClause struct {
	count int
	cols  []*gsql.ResolvedColumn
}

func (vc *viewClause) add(cols []*gsql.ResolvedColumn) {
	if len(cols) == 0 {
		return
	}
	vc.count++
	vc.cols = cols
}

func (s *viewShape) isKey(id int32) bool {
	for !s.keyIDs[id] {
		next, ok := s.renames[id]
		if !ok {
			return false
		}
		id = next
	}
	return true
}

// visit applies production's view rules to n and every node under it, and records the view's shape. A rule on a
// function holds at any depth, in any clause, as production's does.
func (s *viewShape) visit(n gsql.ResolvedNode) error {
	switch n := n.(type) {
	case *gsql.ResolvedLimitOffsetScan:
		return status.Error(codes.InvalidArgument, "Limit and offset are not supported in materialized views.")
	case *gsql.ResolvedFunctionCall:
		name, err := functionName(n)
		if err != nil {
			return err
		}
		if volatile[name] {
			return status.Errorf(codes.InvalidArgument, "Only immutable functions are supported in materialized views (GoogleSQL:%s is not immutable)", name)
		}
	case *gsql.ResolvedAggregateFunctionCall:
		name, err := functionName(n)
		if err != nil {
			return err
		}
		if unstable[name] {
			return status.Errorf(codes.InvalidArgument, "Only stable functions are supported in materialized views (GoogleSQL:%s is not stable)", name)
		}
	case *gsql.ResolvedTableScan:
		idx, err := n.ColumnIndexList()
		if err != nil {
			return internal(err)
		}
		for i, index := range idx {
			if index != 0 {
				continue
			}
			col, err := n.ColumnList2(int32(i))
			if err != nil {
				return internal(err)
			}
			id, err := columnID(col)
			if err != nil {
				return err
			}
			s.keyIDs[id] = true
		}
	case *gsql.ResolvedComputedColumn:
		e, err := n.Expr()
		if err != nil {
			return internal(err)
		}
		if ref, ok := e.(*gsql.ResolvedColumnRef); ok {
			col, err := n.Column()
			if err != nil {
				return internal(err)
			}
			id, err := columnID(col)
			if err != nil {
				return err
			}
			refCol, err := ref.Column()
			if err != nil {
				return internal(err)
			}
			if s.renames[id], err = columnID(refCol); err != nil {
				return err
			}
		}
	case *gsql.ResolvedAggregateScan:
		gb, err := n.GroupByList()
		if err != nil {
			return internal(err)
		}
		var cols []*gsql.ResolvedColumn
		for _, cc := range gb {
			col, err := cc.Column()
			if err != nil {
				return internal(err)
			}
			cols = append(cols, col)
		}
		s.group.add(cols)
	case *gsql.ResolvedOrderByScan:
		items, err := n.OrderByItemList()
		if err != nil {
			return internal(err)
		}
		var cols []*gsql.ResolvedColumn
		for _, it := range items {
			if desc, err := it.IsDescending(); err != nil {
				return internal(err)
			} else if desc {
				return status.Error(codes.InvalidArgument, "Only ascending order in ORDER BY by is supported in materialized views.")
			}
			ref, err := it.ColumnRef()
			if err != nil {
				return internal(err)
			}
			col, err := ref.Column()
			if err != nil {
				return internal(err)
			}
			cols = append(cols, col)
		}
		s.order.add(cols)
	}
	children, err := n.GetChildNodes()
	if err != nil {
		return internal(err)
	}
	for _, c := range children {
		if err := s.visit(c); err != nil {
			return err
		}
	}
	return nil
}

type functionCall interface {
	Function() (*gsql.Function, error)
}

func functionName(call functionCall) (string, error) {
	fn, err := call.Function()
	if err != nil {
		return "", internal(err)
	}
	name, err := fn.Name()
	if err != nil {
		return "", internal(err)
	}
	return name, nil
}

func columnID(c *gsql.ResolvedColumn) (int32, error) {
	id, err := c.ColumnId()
	if err != nil {
		return 0, internal(err)
	}
	return id, nil
}

// viewRules checks a view's resolved query against production's view rules, with production's messages, before the
// engine compiles it, so a rule fails the view before a construct or a type the engine does not support. It returns
// the view's key parts and which output columns the view stores as cells.
func viewRules(qs *gsql.ResolvedQueryStmt) ([]keyPart, []bool, error) {
	s := viewShape{keyIDs: map[int32]bool{}, renames: map[int32]int32{}}
	if err := s.visit(qs); err != nil {
		return nil, nil, err
	}
	if s.group.count > 0 && s.order.count > 0 {
		return nil, nil, status.Error(codes.InvalidArgument, "ORDER BY is not supported with GROUP BY in materialized views.")
	}
	if s.group.count > 1 || s.order.count > 1 {
		return nil, nil, status.Error(codes.InvalidArgument, "This query is not valid. Please ensure that all parts of the query are valid, as per the requirements listed at https://cloud.google.com/bigtable/docs/reference/sql/googlesql-reference-overview. In particular, the query must not use multiple GROUP BY or ORDER BY clauses.")
	}
	cols, clause := s.group.cols, "GROUP BY"
	if cols == nil {
		cols, clause = s.order.cols, "ORDER BY"
	}
	if cols == nil {
		return nil, nil, status.Error(codes.InvalidArgument, "queries must contain a GROUP BY or ORDER BY clause")
	}
	ocs, err := qs.OutputColumnList()
	if err != nil {
		return nil, nil, internal(err)
	}
	outIDs := make([]int32, len(ocs))
	names := make([]string, len(ocs))
	for i, oc := range ocs {
		if names[i], err = oc.Name(); err != nil {
			return nil, nil, internal(err)
		}
		col, err := oc.Column()
		if err != nil {
			return nil, nil, internal(err)
		}
		if outIDs[i], err = columnID(col); err != nil {
			return nil, nil, err
		}
	}
	keys := make([]keyPart, len(cols))
	for i, col := range cols {
		id, t, err := colInfo(col)
		if err != nil {
			return nil, nil, err
		}
		j := slices.Index(outIDs, id)
		if j < 0 {
			return nil, nil, status.Errorf(codes.InvalidArgument, "every %s column must be selected in the final query", clause)
		}
		keys[i] = keyPart{id: id, kind: t.Kind, raw: len(cols) == 1 && names[j] == "_key"}
	}
	isKey := func(k keyPart) bool { return s.isKey(k.id) }
	if clause == "ORDER BY" && !slices.ContainsFunc(keys, isKey) {
		return nil, nil, status.Error(codes.InvalidArgument, "queries must select and order by the unmodified _key column from the source table")
	}
	hint := slices.Contains(names, "_key")
	if clause == "GROUP BY" && hint && !(len(keys) == 1 && isKey(keys[0])) {
		return nil, nil, status.Error(codes.InvalidArgument, "queries that provide a _key hint must only group by _key (and optionally _timestamp). Use a different column name if you want to create a composite key.")
	}
	for _, k := range keys {
		switch k.kind {
		case KindBytes, KindString, KindInt64:
		default:
			return nil, nil, unsupported("a view key that is not BYTES, STRING or INT64")
		}
	}
	loneKey := len(keys) == 1 && keys[0].raw
	cells := make([]bool, len(outIDs))
	for i, id := range outIDs {
		inKey := slices.ContainsFunc(keys, func(k keyPart) bool { return k.id == id })
		cells[i] = !inKey || clause == "ORDER BY" && !loneKey
	}
	return keys, cells, nil
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
	var keys []keyPart
	var cells []bool
	q, err := prepare(sql, sources, nil, func(qs *gsql.ResolvedQueryStmt) (err error) {
		keys, cells, err = viewRules(qs)
		return err
	})
	if err != nil {
		return nil, err
	}
	q.keys, q.cells = keys, cells
	i := slices.IndexFunc(sources, func(t Table) bool { return t.Name == q.Table })
	return &viewPlan{query: q, families: sources[i].Families}, nil
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
