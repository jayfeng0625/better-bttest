// SPDX-License-Identifier: Apache-2.0

//go:build googlesql

package sqlengine

import (
	"bytes"
	"context"
	"testing"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/google/go-cmp/cmp"
)

// recordingSource answers as the server's source does: the rows in the key range, with the requested families' cells.
type recordingSource struct {
	rows  []Row
	scans []ScanRequest
}

func (s *recordingSource) Scan(_ context.Context, req ScanRequest, fn func(Row) error) error {
	s.scans = append(s.scans, req)
	for _, r := range s.rows {
		if bytes.Compare(r.Key, req.Keys.Start) < 0 || req.Keys.End != nil && bytes.Compare(r.Key, req.Keys.End) >= 0 {
			continue
		}
		out := Row{Key: r.Key, Cells: map[string]map[string][]byte{}}
		for _, f := range req.Families {
			if cells := r.Cells[f]; cells != nil {
				out.Cells[f] = cells
			}
		}
		if err := fn(out); err != nil {
			return err
		}
	}
	return nil
}

func TestScanAsksForThePlansFamiliesAndKeyRange(t *testing.T) {
	tables := []Table{{Name: "t", Families: []Family{{Name: "size"}, {Name: "total", Int64: true}}}}
	view, err := CheckView("SELECT _key, size['n'] AS s FROM t ORDER BY _key", tables)
	if err != nil {
		t.Fatalf("CheckView: %v", err)
	}
	tables = append(tables, Table{Name: "v", View: view})
	cell := func(v string) map[string][]byte { return map[string][]byte{"n": []byte(v)} }
	rows := []Row{
		{Key: []byte("a"), Cells: map[string]map[string][]byte{"size": cell("x1")}},
		{Key: []byte("b"), Cells: map[string]map[string][]byte{"size": cell("x2"), "total": cell("\x00\x00\x00\x00\x00\x00\x00\x07")}},
		{Key: []byte("c"), Cells: map[string]map[string][]byte{"total": cell("\x00\x00\x00\x00\x00\x00\x00\x09")}},
		{Key: []byte("d"), Cells: map[string]map[string][]byte{"size": cell("x4")}},
	}
	key := func(k string) []Value { return []Value{{Bytes: []byte(k)}} }
	keys := func(ks ...string) [][]Value {
		var out [][]Value
		for _, k := range ks {
			out = append(out, key(k))
		}
		return out
	}
	b := func(s string) []byte { return []byte(s) }
	bytesType := Type{Kind: KindBytes}.Proto()
	bytesParam := map[string]*btpb.Type{"k": bytesType, "lo": bytesType, "hi": bytesType}
	for _, tc := range []struct {
		name      string
		sql       string
		params    map[string]Value
		wantScans []ScanRequest
		wantRows  [][]Value
	}{
		{
			name:      "the selected family",
			sql:       "SELECT size['n'] AS s FROM t",
			wantScans: []ScanRequest{{Table: "t", Families: []string{"size"}}},
			wantRows:  [][]Value{{{Bytes: b("x1")}}, {{Bytes: b("x2")}}, {null}, {{Bytes: b("x4")}}},
		},
		{
			name:      "the key alone keeps every row",
			sql:       "SELECT _key FROM t",
			wantScans: []ScanRequest{{Table: "t"}},
			wantRows:  keys("a", "b", "c", "d"),
		},
		{
			name:      "both families",
			sql:       "SELECT _key FROM t WHERE size['n'] = b'x2' AND total['n'] = 7",
			wantScans: []ScanRequest{{Table: "t", Families: []string{"size", "total"}}},
			wantRows:  keys("b"),
		},
		{
			name:      "a view reads the view's families",
			sql:       "SELECT _key FROM v WHERE s = b'x4'",
			wantScans: []ScanRequest{{Table: "t", Families: []string{"size"}}},
			wantRows:  keys("d"),
		},
		{
			name:      "key equality",
			sql:       "SELECT _key FROM t WHERE _key = 'b'",
			wantScans: []ScanRequest{{Table: "t", Keys: KeyRange{Start: b("b"), End: b("b\x00")}}},
			wantRows:  keys("b"),
		},
		{
			name:      "key equality with a parameter",
			sql:       "SELECT _key FROM t WHERE _key = @k",
			params:    map[string]Value{"k": {Bytes: b("c")}},
			wantScans: []ScanRequest{{Table: "t", Keys: KeyRange{Start: b("c"), End: b("c\x00")}}},
			wantRows:  keys("c"),
		},
		{
			name:      "key range with parameters",
			sql:       "SELECT _key FROM t WHERE _key >= @lo AND _key < @hi",
			params:    map[string]Value{"lo": {Bytes: b("b")}, "hi": {Bytes: b("d")}},
			wantScans: []ScanRequest{{Table: "t", Keys: KeyRange{Start: b("b"), End: b("d")}}},
			wantRows:  keys("b", "c"),
		},
		{
			name:      "strict bounds",
			sql:       "SELECT _key FROM t WHERE _key > 'a' AND _key <= 'c'",
			wantScans: []ScanRequest{{Table: "t", Keys: KeyRange{Start: b("a\x00"), End: b("c\x00")}}},
			wantRows:  keys("b", "c"),
		},
		{
			name:      "the tightest of two bounds",
			sql:       "SELECT _key FROM t WHERE _key >= 'a' AND _key > 'b' AND _key < 'd' AND _key <= 'd'",
			wantScans: []ScanRequest{{Table: "t", Keys: KeyRange{Start: b("b\x00"), End: b("d")}}},
			wantRows:  keys("c"),
		},
		{
			name:      "between",
			sql:       "SELECT _key FROM t WHERE _key BETWEEN 'b' AND 'c'",
			wantScans: []ScanRequest{{Table: "t", Keys: KeyRange{Start: b("b"), End: b("c\x00")}}},
			wantRows:  keys("b", "c"),
		},
		{
			name:      "the key on the right",
			sql:       "SELECT _key FROM t WHERE 'c' > _key",
			wantScans: []ScanRequest{{Table: "t", Keys: KeyRange{End: b("c")}}},
			wantRows:  keys("a", "b"),
		},
		{
			name:      "a key bound beside another condition",
			sql:       "SELECT _key FROM t WHERE _key >= 'b' AND size['n'] IS NOT NULL",
			wantScans: []ScanRequest{{Table: "t", Families: []string{"size"}, Keys: KeyRange{Start: b("b")}}},
			wantRows:  keys("b", "d"),
		},
		{
			name:      "a key bound under OR narrows nothing",
			sql:       "SELECT _key FROM t WHERE _key = 'a' OR _key = 'c'",
			wantScans: []ScanRequest{{Table: "t"}},
			wantRows:  keys("a", "c"),
		},
		{
			name:      "a NULL key bound reads nothing",
			sql:       "SELECT _key FROM t WHERE _key = @k",
			params:    map[string]Value{"k": null},
			wantScans: nil,
			wantRows:  nil,
		},
		{
			name:      "a strict bound below the empty key reads nothing",
			sql:       "SELECT _key FROM t WHERE _key < b''",
			wantScans: nil,
			wantRows:  nil,
		},
		{
			name:      "no key condition",
			sql:       "SELECT _key FROM t WHERE size['n'] = b'x4'",
			wantScans: []ScanRequest{{Table: "t", Families: []string{"size"}}},
			wantRows:  keys("d"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Prepare(tc.sql, tables, bytesParam)
			if err != nil {
				t.Fatalf("Prepare(%q): %v", tc.sql, err)
			}
			src := &recordingSource{rows: rows}
			var got [][]Value
			if err := q.Run(context.Background(), src, tc.params, func(row []Value) error {
				got = append(got, row)
				return nil
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if diff := cmp.Diff(tc.wantScans, src.scans); diff != "" {
				t.Errorf("scans differ (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantRows, got); diff != "" {
				t.Errorf("rows differ (-want +got):\n%s", diff)
			}
		})
	}
}
