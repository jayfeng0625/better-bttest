// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"strings"
	"testing"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"github.com/google/go-cmp/cmp"
)

// The parity table's families as GetTable returns them, with each aggregate's state type, as the real table's were on
// 2026-10-05.
func gotFamilies() map[string]*adminpb.ColumnFamily {
	got := families()
	for _, f := range got {
		if agg := f.GetValueType().GetAggregateType(); agg != nil {
			agg.StateType = agg.InputType
		}
	}
	return got
}

// Each problem starts with the family's name.
func TestSchemaProblems(t *testing.T) {
	tests := []struct {
		name   string
		change func(map[string]*adminpb.ColumnFamily)
		want   []string
	}{
		{
			name:   "the parity table's families, with their state types",
			change: func(map[string]*adminpb.ColumnFamily) {},
		},
		{
			name:   "a missing family, and a family with another aggregator",
			change: func(f map[string]*adminpb.ColumnFamily) { delete(f, "sum"); f["min"] = f["max"] },
			want:   []string{"min", "sum"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			families := gotFamilies()
			tt.change(families)
			var got []string
			for _, p := range SchemaProblems(families) {
				got = append(got, strings.Fields(p)[0])
			}
			if d := cmp.Diff(tt.want, got); d != "" {
				t.Errorf("families with a problem (-want +got):\n%s", d)
			}
		})
	}
}
