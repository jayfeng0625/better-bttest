// SPDX-License-Identifier: Apache-2.0

package sqlengine

import (
	"encoding/hex"
	"math"
	"strings"
	"testing"
)

// TestEncodeKeyMatchesProductionKeys checks every view key that production returned through ReadRows on
// 2026-10-03, written as hex with spaces between the parts.
func TestEncodeKeyMatchesProductionKeys(t *testing.T) {
	b := func(s string) Value { return Value{Bytes: []byte(s)} }
	i := func(v int64) Value { return Value{Int: v} }
	nul := Value{Null: true}
	multi := []keyPart{{id: 0, kind: KindBytes}, {id: 1, kind: KindInt64}, {id: 2, kind: KindString}}
	ints := []keyPart{{id: 0, kind: KindInt64}}
	byts := []keyPart{{id: 0, kind: KindBytes}}
	order := []keyPart{{id: 0, kind: KindInt64}, {id: 1, kind: KindBytes}}
	rawKey := []keyPart{{id: 0, kind: KindBytes, raw: true}}
	for _, tc := range []struct {
		name string
		keys []keyPart
		row  []Value
		want string
	}{
		{"multi empty min", multi, []Value{b(""), i(math.MinInt64), b("p1")}, "00ff 0001 00ff3f8000ff00ff00ff00ff00ff00ff00ff 0001 7031"},
		{"multi k 0", multi, []Value{b("k"), i(0), b("p1")}, "6b 0001 80 0001 7031"},
		{"multi zero bytes", multi, []Value{b("k\x00\x01"), i(1), b("p\x00")}, "6b00ff01 0001 81 0001 7000ff"},
		{"multi k0a -1", multi, []Value{b("k\x00a"), i(-1), b("p1")}, "6b00ff61 0001 7f 0001 7031"},
		{"multi kff 256", multi, []Value{b("k\xff"), i(256), b("p1")}, "6bff 0001 c100ff 0001 7031"},
		{"multi t1 50", multi, []Value{b("t1"), i(50), b("p1")}, "7431 0001 b2 0001 7031"},
		{"multi t1 70", multi, []Value{b("t1"), i(70), b("p1")}, "7431 0001 c046 0001 7031"},
		{"multi t1 100", multi, []Value{b("t1"), i(100), b("p1")}, "7431 0001 c064 0001 7031"},
		{"multi t5 1", multi, []Value{b("t5"), i(1), b("p1")}, "7435 0001 81 0001 7031"},
		{"multi t5 max", multi, []Value{b("t5"), i(math.MaxInt64), b("p1")}, "7435 0001 ffc07fffffffffffffff 0001 7031"},
		{"multi t6 7", multi, []Value{b("t6"), i(7), b("p1")}, "7436 0001 87 0001 7031"},
		{"multi t7 NULL", multi, []Value{b("t7"), nul, b("p1")}, "7437 0001 0000 0001 7031"},
		{"int NULL", ints, []Value{nul}, "0000"},
		{"int min", ints, []Value{i(math.MinInt64)}, "00ff3f8000ff00ff00ff00ff00ff00ff00ff"},
		{"int -1", ints, []Value{i(-1)}, "7f"},
		{"int 0", ints, []Value{i(0)}, "80"},
		{"int 1", ints, []Value{i(1)}, "81"},
		{"int 7", ints, []Value{i(7)}, "87"},
		{"int 50", ints, []Value{i(50)}, "b2"},
		{"int 70", ints, []Value{i(70)}, "c046"},
		{"int 100", ints, []Value{i(100)}, "c064"},
		{"int 256", ints, []Value{i(256)}, "c100ff"},
		{"int max", ints, []Value{i(math.MaxInt64)}, "ffc07fffffffffffffff"},
		{"bytes empty", byts, []Value{b("")}, "00ff"},
		{"bytes 00", byts, []Value{b("\x00")}, "00ff00ff"},
		{"bytes 0000", byts, []Value{b("\x00\x00")}, "00ff00ff00ff"},
		{"bytes 0001", byts, []Value{b("\x00\x01")}, "00ff01"},
		{"bytes 00a", byts, []Value{b("\x00a")}, "00ff61"},
		{"bytes k", byts, []Value{b("k")}, "6b"},
		{"bytes k0001", byts, []Value{b("k\x00\x01")}, "6b00ff01"},
		{"bytes k00a", byts, []Value{b("k\x00a")}, "6b00ff61"},
		{"bytes kff", byts, []Value{b("k\xff")}, "6bff"},
		{"bytes t1", byts, []Value{b("t1")}, "7431"},
		{"order NULL", order, []Value{nul, b("t7#p1#n#rowJ")}, "0000 0001 7437237031236e23726f774a"},
		{"order min", order, []Value{i(math.MinInt64), b("#p1#n#r5")}, "00ff3f8000ff00ff00ff00ff00ff00ff00ff 0001 237031236e237235"},
		{"order -1", order, []Value{i(-1), b("k\x00a#p1#n#r1")}, "7f 0001 6b00ff61237031236e237231"},
		{"order 1", order, []Value{i(1), b("k\x00\x01#p\x00#n#r3")}, "81 0001 6b00ff01237000ff236e237233"},
		{"order 256", order, []Value{i(256), b("k\xff#p1#n#r4")}, "c100ff 0001 6bff237031236e237234"},
		{"_key output column", rawKey, []Value{b("k\x00\x01#p\x00#n#r3")}, "6b0001237000236e237233"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := hex.EncodeToString(encodeKey(tc.row, tc.keys)), strings.ReplaceAll(tc.want, " ", ""); got != want {
				t.Errorf("encodeKey = %s, want %s", got, want)
			}
		})
	}
}
