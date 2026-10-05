// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"encoding/json"
	"os"
	"testing"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/encoding/protojson"
)

// GetTable's families for the real parity table, read with the Node client on 2026-10-05.
func realFamilies(t *testing.T) map[string]*adminpb.ColumnFamily {
	t.Helper()
	data, err := os.ReadFile("testdata/parity-table-families.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	families := map[string]*adminpb.ColumnFamily{}
	for name, msg := range raw {
		families[name] = &adminpb.ColumnFamily{}
		if err := protojson.Unmarshal(msg, families[name]); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	return families
}

func TestSchemaProblems(t *testing.T) {
	tests := []struct {
		name   string
		change func(map[string]*adminpb.ColumnFamily)
		want   []string
	}{
		{
			name:   "the real parity table has the families the cases need",
			change: func(map[string]*adminpb.ColumnFamily) {},
		},
		{
			name:   "a family with another aggregator",
			change: func(f map[string]*adminpb.ColumnFamily) { f["min"] = f["max"] },
			want:   []string{"min must be an int64 min aggregate family with GC rule never, got an int64 max aggregate family"},
		},
		{
			name: "a family with a GC rule",
			change: func(f map[string]*adminpb.ColumnFamily) {
				f["plain"].GcRule = &adminpb.GcRule{Rule: &adminpb.GcRule_MaxNumVersions{MaxNumVersions: 1}}
			},
			want: []string{"plain must be a family with no value type with GC rule never, got a family with a GC rule"},
		},
		{
			name:   "a missing family",
			change: func(f map[string]*adminpb.ColumnFamily) { delete(f, "sum") },
			want:   []string{"sum must be an int64 sum aggregate family with GC rule never, got no family"},
		},
		{
			name:   "a plain family with an aggregate value type",
			change: func(f map[string]*adminpb.ColumnFamily) { f["plain"] = f["sum"] },
			want:   []string{"plain must be a family with no value type with GC rule never, got an int64 sum aggregate family"},
		},
		{
			name: "a plain family with a value type that is not an aggregate",
			change: func(f map[string]*adminpb.ColumnFamily) {
				f["plain"].ValueType = &adminpb.Type{Kind: &adminpb.Type_StringType{StringType: &adminpb.Type_String{}}}
			},
			want: []string{"plain must be a family with no value type with GC rule never, got a family with another value type"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			families := realFamilies(t)
			tt.change(families)
			if d := cmp.Diff(tt.want, SchemaProblems(families)); d != "" {
				t.Errorf("SchemaProblems (-want +got):\n%s", d)
			}
		})
	}
}
