// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"fmt"
	"maps"
	"slices"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
)

// A Family is one of the parity table's families. The run checks the real table's families, and creates each
// emulator's table with them.
type Family string

const (
	Sum   Family = "sum"
	Min   Family = "min"
	Max   Family = "max"
	Plain Family = "plain"
)

var Aggregates = []Family{Sum, Min, Max}

// The parity table's families: an int64 aggregate family for each aggregator, and a family with no value type. The
// emulators collect garbage every second or so and production does it lazily, so every family has GC rule never and
// keeps every version.
func families() map[string]*adminpb.ColumnFamily {
	aggregate := func(a *adminpb.Type_Aggregate) *adminpb.ColumnFamily {
		a.InputType = &adminpb.Type{Kind: &adminpb.Type_Int64Type{Int64Type: &adminpb.Type_Int64{
			Encoding: &adminpb.Type_Int64_Encoding{Encoding: &adminpb.Type_Int64_Encoding_BigEndianBytes_{}},
		}}}
		return &adminpb.ColumnFamily{ValueType: &adminpb.Type{Kind: &adminpb.Type_AggregateType{AggregateType: a}}}
	}
	return map[string]*adminpb.ColumnFamily{
		string(Sum):   aggregate(&adminpb.Type_Aggregate{Aggregator: &adminpb.Type_Aggregate_Sum_{}}),
		string(Min):   aggregate(&adminpb.Type_Aggregate{Aggregator: &adminpb.Type_Aggregate_Min_{}}),
		string(Max):   aggregate(&adminpb.Type_Aggregate{Aggregator: &adminpb.Type_Aggregate_Max_{}}),
		string(Plain): {},
	}
}

// SchemaProblems returns a diff for each family from GetTable that differs from the parity table's, in family order.
// GetTable adds each aggregate's state type, which the diff leaves out.
func SchemaProblems(got map[string]*adminpb.ColumnFamily) []string {
	var problems []string
	want := families()
	for _, name := range slices.Sorted(maps.Keys(want)) {
		g := proto.CloneOf(got[name])
		if agg := g.GetValueType().GetAggregateType(); agg != nil {
			agg.StateType = nil
		}
		if d := cmp.Diff(want[name], g, protocmp.Transform()); d != "" {
			problems = append(problems, fmt.Sprintf("%s (-want +got):\n%s", name, d))
		}
	}
	return problems
}
