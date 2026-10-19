// SPDX-License-Identifier: Apache-2.0

package sqlengine

import (
	"bytes"
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

// A ScanRequest is what a table scan asks the source for.
type ScanRequest struct {
	Table    string
	Families []string
	Keys     KeyRange
}

// KeyRange is a range of row keys. Start is inclusive, and empty starts at the first row. End is exclusive, and nil
// ends after the last row.
type KeyRange struct {
	Start, End []byte
}

// Source is the seam to storage. Scan calls fn for each row in the key range that holds a cell in any family, in
// ascending key order, with the newest cell of each requested family, and stops with fn's error.
type Source interface {
	Scan(ctx context.Context, req ScanRequest, fn func(Row) error) error
}

type execCtx struct {
	ctx    context.Context
	src    Source
	params map[string]Value
	nSlots int
	// leaveOut makes a view go on without each source row and each group whose evaluation fails, where a plain
	// query fails at the first. A left-out source row leaves nothing in the groups, not even the unnested rows that
	// passed before one failed, so while a view evaluates a source row its sinks stage the rows they keep.
	leaveOut bool
	inRow    bool
	staged   []stagedRow
}

// A sink keeps an evaluated row, as an aggregate adds it to its group or an ORDER BY collects it. It fails when the
// row cannot be kept, as when a SUM overflows.
type sink func(row []Value) error

type stagedRow struct {
	sink sink
	row  []Value
}

// keep passes row to s, or stages a copy of it while a view evaluates a source row.
func (x *execCtx) keep(s sink, row []Value) error {
	if !x.inRow {
		return s(row)
	}
	x.staged = append(x.staged, stagedRow{sink: s, row: slices.Clone(row)})
	return nil
}

// emitSourceRow emits one source row, then passes the rows its sinks staged on to them unless an evaluation failed.
// A staged row that its sink cannot keep leaves out only what the sink marks, such as the overflowing group.
func (x *execCtx) emitSourceRow(emit func([]Value) error, row []Value) error {
	x.inRow = x.leaveOut
	err := emit(row)
	x.inRow = false
	staged := x.staged
	x.staged = x.staged[:0]
	if evalFailure(err) {
		return x.check(err)
	}
	for _, s := range staged {
		if err := x.check(s.sink(s.row)); err != nil {
			return err
		}
	}
	return err
}

// expr evaluates an expression over a row of slots, indexed by resolved column id.
type expr func(x *execCtx, row []Value) (Value, error)

// scan calls emit with each row it produces. A row's slots belong to the scan, which may reuse them for its next row,
// so a caller that keeps a row past emit keeps a copy.
type scan interface {
	run(x *execCtx, emit func(row []Value) error) error
}

// errStop ends a limit scan's input once the limit is reached.
var errStop = errors.New("stop")

// Run executes the query and calls emit with each output row, in output column order.
func (q *Query) Run(ctx context.Context, src Source, params map[string]Value, emit func([]Value) error) error {
	x := &execCtx{ctx: ctx, src: src, params: params, nSlots: q.nSlots}
	return q.root.run(x, func(row []Value) error { return emit(q.output(row)) })
}

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
	// bounds come from the WHERE clause's comparisons of _key. The filter still decides each row.
	bounds []keyBound
}

type keyBound struct {
	value         expr
	strict, upper bool
}

func (s *tableScan) run(x *execCtx, emit func([]Value) error) error {
	keys, nonEmpty, err := s.keyRange(x)
	if err != nil || !nonEmpty {
		return err
	}
	req := ScanRequest{Table: s.table, Keys: keys}
	for _, c := range s.cols {
		if c.family != nil {
			req.Families = append(req.Families, c.family.Name)
		}
	}
	return x.src.Scan(x.ctx, req, func(r Row) error {
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
		return x.emitSourceRow(emit, row)
	})
}

// keyRange is the tightest range the scan's bounds allow. nonEmpty is false when no key can pass: a bound is NULL,
// or _key must be below the empty key.
func (s *tableScan) keyRange(x *execCtx) (r KeyRange, nonEmpty bool, err error) {
	for _, b := range s.bounds {
		v, err := b.value(x, nil)
		if err != nil || v.Null {
			return r, false, err
		}
		// A range holds its Start and stops before its End, so both _key > v and _key <= v put the bound at v's successor.
		key := v.Bytes
		if b.strict != b.upper {
			key = append(slices.Clone(key), 0)
		}
		switch {
		case !b.upper:
			if bytes.Compare(key, r.Start) > 0 {
				r.Start = key
			}
		case len(key) == 0:
			return r, false, nil
		case r.End == nil || bytes.Compare(key, r.End) < 0:
			r.End = key
		}
	}
	return r, true, nil
}

// check drops an evaluation failure in a view, and returns any other error as is.
func (x *execCtx) check(err error) error {
	if x.leaveOut && evalFailure(err) {
		return nil
	}
	return err
}

func evalFailure(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.OutOfRange:
		return true
	}
	return false
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
	collect := func(row []Value) error {
		rows = append(rows, slices.Clone(row))
		return nil
	}
	if err := o.input.run(x, func(row []Value) error {
		return x.keep(collect, row)
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
			row[a.elemID] = el
			if err := emit(row); err != nil {
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
	groupTypes []Type
	groupExprs []expr
	aggs       []aggSpec
}

type group struct {
	row    []Value
	failed bool
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
	nKeys := len(a.groupExprs)
	vals := make([]Value, nKeys+len(a.aggs))
	var key []byte
	add := func(row []Value) error {
		keys, args := row[:nKeys], row[nKeys:]
		key = key[:0]
		for i, v := range keys {
			key = appendGroupKey(key, a.groupTypes[i], v)
		}
		g, ok := groups[string(key)]
		if !ok {
			g = newGroup(keys)
			groups[string(key)] = g
		}
		if g.failed {
			return nil
		}
		for i, ag := range a.aggs {
			if err := ag.add(&g.row[ag.id], args[i]); err != nil {
				g.failed = true
				return err
			}
		}
		return nil
	}
	err := a.input.run(x, func(row []Value) error {
		// Evaluate everything the row adds before changing any group, so a failing row changes no group.
		for i, g := range a.groupExprs {
			v, err := g(x, row)
			if err != nil {
				return err
			}
			vals[i] = v
		}
		for i, ag := range a.aggs {
			if ag.arg == nil {
				continue
			}
			v, err := ag.arg(x, row)
			if err != nil {
				return err
			}
			vals[nKeys+i] = v
		}
		return x.keep(add, vals)
	})
	if err != nil {
		return err
	}
	for _, g := range order {
		if g.failed {
			continue
		}
		if err := x.check(emit(g.row)); err != nil {
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
