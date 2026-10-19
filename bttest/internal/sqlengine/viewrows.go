// SPDX-License-Identifier: Apache-2.0

package sqlengine

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/bits"
	"slices"
	"strings"
)

// keyPart is one part of a materialized view's row key: the slot that holds it, its kind, and whether it is an
// output column named _key that makes up the whole key, which production takes as the key unencoded.
type keyPart struct {
	id   int32
	kind Kind
	raw  bool
}

// viewScan reads a view. Each column's index is the view's output column index.
type viewScan struct {
	view *Query
	cols []scanCol
}

func (s *viewScan) run(x *execCtx, emit func([]Value) error) error {
	return s.view.viewRows(x.ctx, x.src, func(_ []byte, out []Value) error {
		row := make([]Value, x.nSlots)
		for _, c := range s.cols {
			row[c.id] = out[c.index]
		}
		return x.check(emit(row))
	})
}

// viewRows evaluates the view's query as production maintains the view: it leaves out each source row and each
// group whose evaluation fails, and passes each row's encoded key and output values to emit in key order.
func (q *Query) viewRows(ctx context.Context, src Source, emit func(key []byte, out []Value) error) error {
	vx := &execCtx{ctx: ctx, src: src, nSlots: q.nSlots, leaveOut: true}
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
		if err := emit(r.key, r.row); err != nil {
			return err
		}
	}
	return nil
}

// A ViewCell is one cell of a materialized view row as production stores it: the output column's name, and its
// value as bytes, an INT64 as 8 big-endian bytes.
type ViewCell struct {
	Column string
	Value  []byte
}

// ViewRows passes each row of the view to emit in key order, as production's ReadRows returns them: the encoded key,
// and the cells in column name order. A NULL value has no cell. Production keeps a GROUP BY view's group columns in
// the key only, and stores an ORDER BY view's columns as cells unless the view's whole key is the _key column.
func (q *Query) ViewRows(ctx context.Context, src Source, emit func(key []byte, cells []ViewCell) error) error {
	return q.viewRows(ctx, src, func(key []byte, out []Value) error {
		var cells []ViewCell
		for i, v := range out {
			if v.Null || !q.cells[i] {
				continue
			}
			c := ViewCell{Column: q.Columns[i].Name}
			switch q.Columns[i].Type.Kind {
			case KindInt64:
				c.Value = binary.BigEndian.AppendUint64(nil, uint64(v.Int))
			case KindBytes, KindString:
				c.Value = v.Bytes
			default:
				return unsupported("ReadRows on a view with a column that is not BYTES, STRING or INT64")
			}
			cells = append(cells, c)
		}
		slices.SortFunc(cells, func(a, b ViewCell) int { return strings.Compare(a.Column, b.Column) })
		return emit(key, cells)
	})
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
