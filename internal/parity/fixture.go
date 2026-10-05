// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"fmt"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
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

// Each family's aggregator, or "" for a family with no value type. Every family has GC rule never.
var families = []struct {
	name       Family
	aggregator string
}{
	{Sum, "sum"},
	{Min, "min"},
	{Max, "max"},
	{Plain, ""},
}

func describe(aggregator string) string {
	if aggregator == "" {
		return "a family with no value type"
	}
	return fmt.Sprintf("an int64 %s aggregate family", aggregator)
}

// What a family from GetTable is, in describe's words where it can be.
func describeFamily(f *adminpb.ColumnFamily) string {
	switch {
	case f == nil:
		return "no family"
	case f.GcRule != nil:
		return "a family with a GC rule"
	case f.ValueType == nil:
		return describe("")
	}
	agg := f.ValueType.GetAggregateType()
	if agg.GetInputType().GetInt64Type() == nil {
		return "a family with another value type"
	}
	switch agg.Aggregator.(type) {
	case *adminpb.Type_Aggregate_Sum_:
		return describe("sum")
	case *adminpb.Type_Aggregate_Min_:
		return describe("min")
	case *adminpb.Type_Aggregate_Max_:
		return describe("max")
	}
	return "a family with another value type"
}

// The families that CreateTable takes for the parity table.
func columnFamilies() map[string]*adminpb.ColumnFamily {
	out := map[string]*adminpb.ColumnFamily{}
	for _, f := range families {
		cf := &adminpb.ColumnFamily{}
		if f.aggregator != "" {
			agg := &adminpb.Type_Aggregate{InputType: &adminpb.Type{Kind: &adminpb.Type_Int64Type{Int64Type: &adminpb.Type_Int64{
				Encoding: &adminpb.Type_Int64_Encoding{Encoding: &adminpb.Type_Int64_Encoding_BigEndianBytes_{}},
			}}}}
			switch f.aggregator {
			case "sum":
				agg.Aggregator = &adminpb.Type_Aggregate_Sum_{}
			case "min":
				agg.Aggregator = &adminpb.Type_Aggregate_Min_{}
			case "max":
				agg.Aggregator = &adminpb.Type_Aggregate_Max_{}
			}
			cf.ValueType = &adminpb.Type{Kind: &adminpb.Type_AggregateType{AggregateType: agg}}
		}
		out[string(f.name)] = cf
	}
	return out
}

// SchemaProblems returns each way the families from GetTable differ from the parity table's.
func SchemaProblems(got map[string]*adminpb.ColumnFamily) []string {
	var problems []string
	for _, f := range families {
		want := describe(f.aggregator)
		if actual := describeFamily(got[string(f.name)]); actual != want {
			problems = append(problems, fmt.Sprintf("%s must be %s with GC rule never, got %s", f.name, want, actual))
		}
	}
	return problems
}
