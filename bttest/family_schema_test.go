// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"context"
	"testing"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
)

// Production fills in the state type of a Sum, MIN, or MAX family with its input type, as a parity run recorded.
func TestGetTableReturnsAggregateStateType(t *testing.T) {
	s := &server{tables: make(map[string]*table)}
	name := createFamilies(t, s, map[string]*btapb.ColumnFamily{
		"sum": int64Family(&btapb.Type_Aggregate{Aggregator: &btapb.Type_Aggregate_Sum_{}}, nil),
		"min": int64Family(&btapb.Type_Aggregate{Aggregator: &btapb.Type_Aggregate_Min_{}}, nil),
		"max": int64Family(&btapb.Type_Aggregate{Aggregator: &btapb.Type_Aggregate_Max_{}}, nil),
	})
	want := map[string]*btapb.ColumnFamily{
		"sum": int64Family(&btapb.Type_Aggregate{Aggregator: &btapb.Type_Aggregate_Sum_{}}, bigEndianInt64()),
		"min": int64Family(&btapb.Type_Aggregate{Aggregator: &btapb.Type_Aggregate_Min_{}}, bigEndianInt64()),
		"max": int64Family(&btapb.Type_Aggregate{Aggregator: &btapb.Type_Aggregate_Max_{}}, bigEndianInt64()),
	}
	if d := cmp.Diff(want, getFamilies(t, s, name), protocmp.Transform()); d != "" {
		t.Errorf("families (-want +got):\n%s", d)
	}
}

// Production stores an empty GC rule, one with no rule in it, as no rule, whether CreateTable or ModifyColumnFamilies
// sends it, as a parity run recorded.
func TestGetTableReturnsEmptyGCRuleAsNoRule(t *testing.T) {
	empty := func() *btapb.ColumnFamily { return &btapb.ColumnFamily{GcRule: &btapb.GcRule{}} }
	oneVersion := func() *btapb.ColumnFamily {
		return &btapb.ColumnFamily{GcRule: &btapb.GcRule{Rule: &btapb.GcRule_MaxNumVersions{MaxNumVersions: 1}}}
	}
	s := &server{tables: make(map[string]*table)}
	name := createFamilies(t, s, map[string]*btapb.ColumnFamily{"cf": oneVersion(), "created": empty()})

	modify := func(m *btapb.ModifyColumnFamiliesRequest_Modification) {
		t.Helper()
		if _, err := s.ModifyColumnFamilies(context.Background(), &btapb.ModifyColumnFamiliesRequest{
			Name: name, Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{m},
		}); err != nil {
			t.Fatal(err)
		}
	}
	modify(&btapb.ModifyColumnFamiliesRequest_Modification{Id: "added", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: empty()}})
	want := map[string]*btapb.ColumnFamily{"cf": oneVersion(), "created": {}, "added": {}}
	if d := cmp.Diff(want, getFamilies(t, s, name), protocmp.Transform()); d != "" {
		t.Errorf("after CreateTable and a create: families (-want +got):\n%s", d)
	}

	modify(&btapb.ModifyColumnFamiliesRequest_Modification{Id: "cf", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Update{Update: empty()}})
	want["cf"] = &btapb.ColumnFamily{}
	if d := cmp.Diff(want, getFamilies(t, s, name), protocmp.Transform()); d != "" {
		t.Errorf("after an update: families (-want +got):\n%s", d)
	}
}

func bigEndianInt64() *btapb.Type {
	return &btapb.Type{Kind: &btapb.Type_Int64Type{Int64Type: &btapb.Type_Int64{
		Encoding: &btapb.Type_Int64_Encoding{Encoding: &btapb.Type_Int64_Encoding_BigEndianBytes_{}},
	}}}
}

// int64Family is a family of the aggregate a over big-endian int64 inputs, with the state type state.
func int64Family(a *btapb.Type_Aggregate, state *btapb.Type) *btapb.ColumnFamily {
	a.InputType, a.StateType = bigEndianInt64(), state
	return &btapb.ColumnFamily{ValueType: &btapb.Type{Kind: &btapb.Type_AggregateType{AggregateType: a}}}
}

func createFamilies(t *testing.T, s *server, families map[string]*btapb.ColumnFamily) string {
	t.Helper()
	tbl, err := s.CreateTable(context.Background(), &btapb.CreateTableRequest{
		Parent: "projects/p/instances/i", TableId: "t", Table: &btapb.Table{ColumnFamilies: families},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tbl.Name
}

func getFamilies(t *testing.T, s *server, name string) map[string]*btapb.ColumnFamily {
	t.Helper()
	tbl, err := s.GetTable(context.Background(), &btapb.GetTableRequest{Name: name, View: btapb.Table_SCHEMA_VIEW})
	if err != nil {
		t.Fatal(err)
	}
	return tbl.ColumnFamilies
}
