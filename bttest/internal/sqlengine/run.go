// SPDX-License-Identifier: Apache-2.0

package sqlengine

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Row is one source row: its key and the newest value of each column, by family then qualifier.
type Row struct {
	Key   []byte
	Cells map[string]map[string][]byte
}

// Source is the seam to storage. Scan calls fn for each row of the table that holds a cell, in ascending key
// order, and stops with fn's error.
type Source interface {
	Scan(ctx context.Context, table string, fn func(Row) error) error
}

type execCtx struct {
	ctx    context.Context
	src    Source
	params map[string]Value
	nSlots int
	// failed receives each source row's and each group's evaluation failure, and returns the error that ends the
	// query, or nil to go on without that row or group. A plain query fails at the first.
	failed func(error) error
}

// expr evaluates an expression over a row of slots, indexed by resolved column id.
type expr func(x *execCtx, row []Value) (Value, error)

type scan interface {
	run(x *execCtx, emit func(row []Value) error) error
}

// errStop ends a limit scan's input once the limit is reached.
var errStop = errors.New("stop")

// Run executes the query and calls emit with each output row, in output column order.
func (q *Query) Run(ctx context.Context, src Source, params map[string]Value, emit func([]Value) error) error {
	x := &execCtx{ctx: ctx, src: src, params: params, nSlots: q.nSlots, failed: func(err error) error { return err }}
	return q.root.run(x, func(row []Value) error { return emit(q.output(row)) })
}

// output returns a row's output columns, in output column order.
func (q *Query) output(row []Value) []Value {
	out := make([]Value, len(q.outIDs))
	for i, id := range q.outIDs {
		out[i] = row[id]
	}
	return out
}

// tableCol is one column a table scan reads: _key when family is nil.
type tableCol struct {
	id     int32
	family *Family
}

type tableScan struct {
	table string
	cols  []tableCol
}

func (s *tableScan) run(x *execCtx, emit func([]Value) error) error {
	return x.src.Scan(x.ctx, s.table, func(r Row) error {
		if err := x.ctx.Err(); err != nil {
			return err
		}
		row := make([]Value, x.nSlots)
		for _, c := range s.cols {
			if c.family == nil {
				row[c.id] = Value{Bytes: r.Key}
				continue
			}
			entries := map[string]Value{}
			for q, v := range r.Cells[c.family.Name] {
				if !c.family.Int64 {
					entries[q] = Value{Bytes: v}
					continue
				}
				if len(v) != 8 {
					return status.Errorf(codes.Internal, "cell %s:%s of row %q holds %d bytes, want an 8-byte INT64", c.family.Name, q, r.Key, len(v))
				}
				entries[q] = Value{Int: int64(binary.BigEndian.Uint64(v))}
			}
			row[c.id] = newMap(entries)
		}
		return x.check(emit(row))
	})
}

// check passes an evaluation failure to x.failed, and returns any other error as is.
func (x *execCtx) check(err error) error {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.OutOfRange:
		return x.failed(err)
	}
	return err
}

type filter struct {
	input scan
	pred  expr
}

func (f *filter) run(x *execCtx, emit func([]Value) error) error {
	return f.input.run(x, func(row []Value) error {
		v, err := f.pred(x, row)
		if err != nil {
			return err
		}
		if v.Null || !v.Bool {
			return nil
		}
		return emit(row)
	})
}

type project struct {
	input scan
	ids   []int32
	exprs []expr
}

func (p *project) run(x *execCtx, emit func([]Value) error) error {
	return p.input.run(x, func(row []Value) error {
		for i, e := range p.exprs {
			v, err := e(x, row)
			if err != nil {
				return err
			}
			row[p.ids[i]] = v
		}
		return emit(row)
	})
}

type sortKey struct {
	id         int32
	kind       Kind
	desc       bool
	nullsFirst bool
}

type orderBy struct {
	input scan
	keys  []sortKey
}

func (o *orderBy) run(x *execCtx, emit func([]Value) error) error {
	var rows [][]Value
	if err := o.input.run(x, func(row []Value) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		return err
	}
	slices.SortStableFunc(rows, func(a, b []Value) int {
		for _, k := range o.keys {
			if c := k.compare(a[k.id], b[k.id]); c != 0 {
				return c
			}
		}
		return 0
	})
	for _, r := range rows {
		if err := emit(r); err != nil {
			return err
		}
	}
	return nil
}

func (k sortKey) compare(a, b Value) int {
	nullFirst := 1
	if k.nullsFirst {
		nullFirst = -1
	}
	switch {
	case a.Null && b.Null:
		return 0
	case a.Null:
		return nullFirst
	case b.Null:
		return -nullFirst
	}
	c := compare(k.kind, a, b)
	if k.desc {
		return -c
	}
	return c
}

type limitScan struct {
	input scan
	limit expr
}

func (l *limitScan) run(x *execCtx, emit func([]Value) error) error {
	v, err := l.limit(x, nil)
	if err != nil {
		return err
	}
	if v.Null {
		return status.Error(codes.OutOfRange, "Limit requires non-null count and offset")
	}
	if v.Int < 0 {
		return status.Error(codes.OutOfRange, "Limit requires non-negative count and offset")
	}
	if v.Int == 0 {
		return nil
	}
	var sent int64
	err = l.input.run(x, func(row []Value) error {
		if err := emit(row); err != nil {
			return err
		}
		if sent++; sent >= v.Int {
			return errStop
		}
		return nil
	})
	// The stop ends only this scan's input, so an enclosing scan still emits its rows.
	if errors.Is(err, errStop) {
		return nil
	}
	return err
}

// arrayScan joins each input row with each element of an array. A NULL or empty array gives no rows.
type arrayScan struct {
	input  scan
	array  expr
	elemID int32
}

func (a *arrayScan) run(x *execCtx, emit func([]Value) error) error {
	return a.input.run(x, func(row []Value) error {
		arr, err := a.array(x, row)
		if err != nil {
			return err
		}
		for _, el := range arr.Elems {
			// Each output row gets its own slots, since a later scan may keep it.
			out := slices.Clone(row)
			out[a.elemID] = el
			if err := emit(out); err != nil {
				return err
			}
		}
		return nil
	})
}

type aggFunc int

const (
	aggCountStar aggFunc = iota
	aggSum
	aggMax
)

// aggSpec is one aggregate: its function, its argument when it has one, and the output column.
type aggSpec struct {
	id   int32
	kind Kind
	fn   aggFunc
	arg  expr
}

// aggregate groups its input by the group expressions and computes each aggregate per group. It emits the groups
// in the order each first appears. With no GROUP BY it emits one group, even for no input.
type aggregate struct {
	input      scan
	groupIDs   []int32
	groupExprs []expr
	aggs       []aggSpec
}

type group struct {
	row []Value
	err error // the first failure while accumulating, such as SUM overflow
}

func (a *aggregate) run(x *execCtx, emit func([]Value) error) error {
	groups := map[string]*group{}
	var order []*group
	newGroup := func(keys []Value) *group {
		g := &group{row: make([]Value, x.nSlots)}
		for i, id := range a.groupIDs {
			g.row[id] = keys[i]
		}
		for _, ag := range a.aggs {
			if ag.fn == aggCountStar {
				g.row[ag.id] = Value{}
			} else {
				g.row[ag.id] = null
			}
		}
		order = append(order, g)
		return g
	}
	if len(a.groupExprs) == 0 {
		groups[""] = newGroup(nil)
	}
	keys := make([]Value, len(a.groupExprs))
	args := make([]Value, len(a.aggs))
	err := a.input.run(x, func(row []Value) error {
		// Evaluate everything the row adds before changing any group, so a failing row leaves no trace.
		var key []byte
		for i, g := range a.groupExprs {
			v, err := g(x, row)
			if err != nil {
				return err
			}
			keys[i] = v
			key = appendGroupKey(key, v)
		}
		for i, ag := range a.aggs {
			if ag.arg == nil {
				continue
			}
			v, err := ag.arg(x, row)
			if err != nil {
				return err
			}
			args[i] = v
		}
		g, ok := groups[string(key)]
		if !ok {
			g = newGroup(keys)
			groups[string(key)] = g
		}
		if g.err != nil {
			return nil
		}
		for i, ag := range a.aggs {
			if g.err = ag.add(&g.row[ag.id], args[i]); g.err != nil {
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, g := range order {
		err := g.err
		if err == nil {
			err = emit(g.row)
		}
		if err := x.check(err); err != nil {
			return err
		}
	}
	return nil
}

func (ag aggSpec) add(acc *Value, v Value) error {
	if ag.fn == aggCountStar {
		acc.Int++
		return nil
	}
	if v.Null {
		return nil
	}
	if acc.Null {
		*acc = v
		return nil
	}
	switch ag.fn {
	case aggSum:
		s := acc.Int + v.Int
		if (v.Int > 0 && s < acc.Int) || (v.Int < 0 && s > acc.Int) {
			return status.Error(codes.OutOfRange, "SUM() aggregation overflow")
		}
		acc.Int = s
	case aggMax:
		if compare(ag.kind, v, *acc) > 0 {
			*acc = v
		}
	}
	return nil
}
