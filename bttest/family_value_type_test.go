// SPDX-License-Identifier: Apache-2.0

package bttest_test

import (
	"context"
	"testing"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const immutableValueType = "Immutable fields 'value_type.aggregate_type,sql_type' cannot be updated."

func int64Aggregate(a *btapb.Type_Aggregate) *btapb.ColumnFamily {
	a.InputType = &btapb.Type{Kind: &btapb.Type_Int64Type{Int64Type: &btapb.Type_Int64{
		Encoding: &btapb.Type_Int64_Encoding{Encoding: &btapb.Type_Int64_Encoding_BigEndianBytes_{}},
	}}}
	return &btapb.ColumnFamily{ValueType: &btapb.Type{Kind: &btapb.Type_AggregateType{AggregateType: a}}}
}

func sumFamily() *btapb.ColumnFamily {
	return int64Aggregate(&btapb.Type_Aggregate{Aggregator: &btapb.Type_Aggregate_Sum_{}})
}

func updateFamily(id string, cf *btapb.ColumnFamily, paths ...string) *btapb.ModifyColumnFamiliesRequest_Modification {
	return &btapb.ModifyColumnFamiliesRequest_Modification{
		Id: id, Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Update{Update: cf}, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
	}
}

// Production refuses a ModifyColumnFamilies request that changes a family's value type, and applies none of its
// modifications. An update to the family's own type succeeds, as a parity run recorded.
func TestModifyColumnFamiliesRefusesAValueTypeChange(t *testing.T) {
	oneVersion := sumFamily()
	oneVersion.GcRule = &btapb.GcRule{Rule: &btapb.GcRule_MaxNumVersions{MaxNumVersions: 1}}
	addCF2 := &btapb.ModifyColumnFamiliesRequest_Modification{
		Id: "cf2", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}},
	}
	for _, tc := range []struct {
		name    string
		mods    []*btapb.ModifyColumnFamiliesRequest_Modification
		refused bool
	}{
		{"plain to Sum", []*btapb.ModifyColumnFamiliesRequest_Modification{updateFamily("plain", sumFamily(), "value_type")}, true},
		{"Sum to MIN", []*btapb.ModifyColumnFamiliesRequest_Modification{
			updateFamily("sum", int64Aggregate(&btapb.Type_Aggregate{Aggregator: &btapb.Type_Aggregate_Min_{}}), "value_type"),
		}, true},
		{"Sum to plain", []*btapb.ModifyColumnFamiliesRequest_Modification{updateFamily("sum", &btapb.ColumnFamily{}, "value_type")}, true},
		{"plain to Sum with a GC rule", []*btapb.ModifyColumnFamiliesRequest_Modification{updateFamily("plain", oneVersion, "gc_rule", "value_type")}, true},
		{"plain to Sum after a create", []*btapb.ModifyColumnFamiliesRequest_Modification{addCF2, updateFamily("plain", sumFamily(), "value_type")}, true},
		{"plain to plain", []*btapb.ModifyColumnFamiliesRequest_Modification{updateFamily("plain", &btapb.ColumnFamily{}, "value_type")}, false},
		{"Sum to Sum", []*btapb.ModifyColumnFamiliesRequest_Modification{updateFamily("sum", sumFamily(), "value_type")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			admin := btapb.NewBigtableTableAdminClient(newConn(t))
			tbl, err := admin.CreateTable(ctx, &btapb.CreateTableRequest{
				Parent: "projects/p/instances/i", TableId: "t",
				Table: &btapb.Table{ColumnFamilies: map[string]*btapb.ColumnFamily{"plain": {}, "sum": sumFamily()}},
			})
			if err != nil {
				t.Fatal(err)
			}
			families := func() map[string]*btapb.ColumnFamily {
				t.Helper()
				got, err := admin.GetTable(ctx, &btapb.GetTableRequest{Name: tbl.Name, View: btapb.Table_SCHEMA_VIEW})
				if err != nil {
					t.Fatal(err)
				}
				return got.ColumnFamilies
			}
			before := families()

			_, err = admin.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{Name: tbl.Name, Modifications: tc.mods})
			if !tc.refused {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantStatus(t, err, codes.InvalidArgument, immutableValueType)
			if d := cmp.Diff(before, families(), protocmp.Transform()); d != "" {
				t.Errorf("families after the refused request (-want +got):\n%s", d)
			}
		})
	}
}
