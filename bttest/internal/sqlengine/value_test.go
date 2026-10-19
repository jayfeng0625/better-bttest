// SPDX-License-Identifier: Apache-2.0

package sqlengine

import "testing"

func TestTypeEqual(t *testing.T) {
	bytesT := Type{Kind: KindBytes}
	stringT := Type{Kind: KindString}
	int64T := Type{Kind: KindInt64}
	array := func(elem Type) Type { return Type{Kind: KindArray, Elem: &elem} }
	mapOf := func(key, val Type) Type { return Type{Kind: KindMap, Key: &key, Elem: &val} }

	for _, tc := range []struct {
		name string
		a, b Type
		want bool
	}{
		{"BYTES equals BYTES", bytesT, Type{Kind: KindBytes}, true},
		{"BYTES differs from STRING", bytesT, stringT, false},
		{"ARRAY<STRING> equals ARRAY<STRING>", array(stringT), array(Type{Kind: KindString}), true},
		{"ARRAY<STRING> differs from ARRAY<BYTES>", array(stringT), array(bytesT), false},
		{"ARRAY<STRING> differs from STRING", array(stringT), stringT, false},
		{"MAP<BYTES, BYTES> equals MAP<BYTES, BYTES>", mapOf(bytesT, bytesT), mapOf(bytesT, bytesT), true},
		{"MAP<BYTES, BYTES> differs from MAP<BYTES, INT64>", mapOf(bytesT, bytesT), mapOf(bytesT, int64T), false},
		{"MAP<BYTES, INT64> differs from MAP<STRING, INT64>", mapOf(bytesT, int64T), mapOf(stringT, int64T), false},
		{"MAP<BYTES, BYTES> differs from ARRAY<BYTES>", mapOf(bytesT, bytesT), array(bytesT), false},
		{"ARRAY<ARRAY<STRING>> differs from ARRAY<ARRAY<BYTES>>", array(array(stringT)), array(array(bytesT)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Equal(tc.b); got != tc.want {
				t.Errorf("Equal() = %v, want %v", got, tc.want)
			}
			if got := tc.b.Equal(tc.a); got != tc.want {
				t.Errorf("reversed Equal() = %v, want %v", got, tc.want)
			}
		})
	}
}
