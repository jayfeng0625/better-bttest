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
	x := &execCtx{ctx: ctx, src: src, params: params, nSlots: q.nSlots}
	return q.root.run(x, func(row []Value) error {
		out := make([]Value, len(q.outIDs))
		for i, id := range q.outIDs {
			out[i] = row[id]
		}
		return emit(out)
	})
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
		return emit(row)
	})
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
