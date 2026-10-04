// SPDX-License-Identifier: Apache-2.0

package sqlengine

import (
	"bytes"
	"encoding/binary"
	"math/bits"
	"slices"

	gsql "github.com/goccy/go-googlesql"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// keyPart is one part of a materialized view's row key: the slot that holds it, its kind, and whether it is an
// output column named _key, which production takes as the key unencoded.
type keyPart struct {
	id   int32
	kind Kind
	raw  bool
}

// viewKeys is what the compiler records of a view's query: the key parts of its outermost GROUP BY and ORDER BY.
type viewKeys struct {
	groupKeys []keyPart
	orderKeys []keyPart
}

// unstable names the aggregates production rejects in a view as not stable.
var unstable = map[string]bool{"any_value": true}

// PrepareView analyzes and compiles a materialized view's query against the tables, and checks it against
// production's view rules with production's messages. A view reads tables only, so a view over a view fails as a
// missing table.
func PrepareView(sql string, tables []Table) (*Query, error) {
	var sources []Table
	for _, t := range tables {
		if t.ViewQuery == "" {
			sources = append(sources, t)
		}
	}
	c := &compiler{view: true}
	q, err := prepare(sql, sources, nil, c)
	if err != nil {
		return nil, err
	}
	keys := c.keys.groupKeys
	if keys == nil {
		keys = c.keys.orderKeys
	}
	if keys == nil {
		return nil, status.Error(codes.InvalidArgument, "queries must contain a GROUP BY or ORDER BY clause")
	}
	for i, k := range keys {
		switch k.kind {
		case KindBytes, KindString, KindInt64:
		default:
			return nil, unsupported("a view key that is not BYTES, STRING or INT64")
		}
		for j, id := range q.outIDs {
			if id == k.id && q.Columns[j].Name == "_key" {
				keys[i].raw = true
			}
		}
	}
	q.keys = keys
	return q, nil
}

// addView prepares a view's query and adds the view to the catalog as a table of its output columns.
func (e *env) addView(t Table, tables []Table) error {
	v, err := PrepareView(t.ViewQuery, tables)
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
func (c *compiler) viewScan(s *gsql.ResolvedTableScan, v *Query) (scan, error) {
	c.q.Table = v.Table
	c.q.Families = append(c.q.Families, v.Families...)
	cols, err := scanColumns(s)
	if err != nil {
		return nil, err
	}
	return &viewScan{view: v, cols: cols}, nil
}

// viewScan reads a view. Each column's index is the view's output column index.
type viewScan struct {
	view *Query
	cols []scanCol
}

// run evaluates the view's query as production maintains the view: it leaves out each source row and each group
// whose evaluation fails, and orders the rows by their encoded keys.
func (s *viewScan) run(x *execCtx, emit func([]Value) error) error {
	q := s.view
	vx := &execCtx{ctx: x.ctx, src: x.src, nSlots: q.nSlots, failed: func(error) error { return nil }}
	type keyedRow struct {
		key []byte
		row []Value
	}
	var rows []keyedRow
	if err := q.root.run(vx, func(row []Value) error {
		rows = append(rows, keyedRow{key: encodeKey(row, q.keys), row: q.output(row)})
		return nil
	}); err != nil {
		return err
	}
	slices.SortStableFunc(rows, func(a, b keyedRow) int { return bytes.Compare(a.key, b.key) })
	for _, r := range rows {
		row := make([]Value, x.nSlots)
		for _, c := range s.cols {
			row[c.id] = r.row[c.index]
		}
		if err := x.check(emit(row)); err != nil {
			return err
		}
	}
	return nil
}

// encodeKey returns a view row's key as production stores it: the parts joined with \x00\x01.
func encodeKey(row []Value, keys []keyPart) []byte {
	var key []byte
	for i, k := range keys {
		if i > 0 {
			key = append(key, 0, 1)
		}
		if k.raw {
			key = append(key, row[k.id].Bytes...)
		} else {
			key = appendKeyPart(key, row[k.id], k.kind)
		}
	}
	return key
}

// appendKeyPart appends a view key part as production encodes it. NULL is \x00\x00. INT64 is OrderedCode's
// increasing signed number. Each \x00 in the part becomes \x00\xff, and a BYTES or STRING value made only of \x00
// bytes, the empty value included, gets one more \x00\xff.
func appendKeyPart(b []byte, v Value, k Kind) []byte {
	if v.Null {
		return append(b, 0, 0)
	}
	raw := v.Bytes
	if k == KindInt64 {
		raw = appendSignedIncreasing(nil, v.Int)
	}
	allZero := true
	for _, c := range raw {
		if c == 0 {
			b = append(b, 0, 0xff)
			continue
		}
		allZero = false
		b = append(b, c)
	}
	if allZero && k != KindInt64 {
		b = append(b, 0, 0xff)
	}
	return b
}

// lengthHeader holds the bits OrderedCode sets in the first two bytes of a signed number of each length.
var lengthHeader = [11][2]byte{{0, 0}, {0x80, 0}, {0xc0, 0}, {0xe0, 0}, {0xf0, 0}, {0xf8, 0}, {0xfc, 0}, {0xfe, 0}, {0xff, 0}, {0xff, 0x80}, {0xff, 0xc0}}

// appendSignedIncreasing appends v in OrderedCode's WriteSignedNumIncreasing encoding: 1 to 10 bytes whose leading
// bits give the length, and whose byte order is the numeric order.
func appendSignedIncreasing(b []byte, v int64) []byte {
	x := uint64(v)
	if v < 0 {
		x = ^x
	}
	if x < 64 {
		return append(b, 0x80^byte(v))
	}
	n := bits.Len64(x)/7 + 1
	var buf [10]byte
	if v < 0 {
		buf[0], buf[1] = 0xff, 0xff
	}
	binary.BigEndian.PutUint64(buf[2:], uint64(v))
	p := buf[10-n:]
	p[0] ^= lengthHeader[n][0]
	p[1] ^= lengthHeader[n][1]
	return append(b, p...)
}
