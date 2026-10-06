// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"testing"
	"time"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/google/go-cmp/cmp"
)

// A run that is still going keeps its rows, views and tables. An interrupted run's are deleted with the run's own.
func TestCleanupDeletesTheRunsAndStaleRunsData(t *testing.T) {
	ctx, target := startGate(t)
	run := NewRunID(time.Now())
	other := NewRunID(time.Now())
	for other == run {
		other = NewRunID(time.Now())
	}
	stale := NewRunID(time.Now().Add(-2 * time.Hour))
	write := func(key string) {
		t.Helper()
		_, err := target.Data.MutateRow(ctx, &btpb.MutateRowRequest{
			TableName: target.tablePath(parityTable), RowKey: []byte(key), Mutations: Mutations(SetCell(Plain, BE(1))),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	create := func(id string, protected bool) {
		t.Helper()
		if _, err := target.Admin.CreateTable(ctx, &adminpb.CreateTableRequest{Parent: target.Instance, TableId: id}); err != nil {
			t.Fatal(err)
		}
		if err := target.updateTable(ctx, &adminpb.Table{Name: target.tablePath(id), DeletionProtection: protected}, "deletion_protection", false); err != nil {
			t.Fatal(err)
		}
	}
	view := func(id string) {
		t.Helper()
		_, err := target.Instances.CreateMaterializedView(ctx, &adminpb.CreateMaterializedViewRequest{
			Parent: target.Instance, MaterializedViewId: tablePrefix + id + "-v1",
			MaterializedView: &adminpb.MaterializedView{Query: "SELECT _key AS k FROM `" + tablePrefix + id + "-t1` ORDER BY k", DeletionProtection: true},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{run, other, stale} {
		write(rowPrefix + id + "#case")
		create(tablePrefix+id+"-t1", true)
		view(id)
	}
	write("not a case row")

	if err := Cleanup(ctx, target, run); err != nil {
		t.Fatal(err)
	}

	everyRun := func(string) bool { return true }
	rows, err := caseRows(ctx, target, everyRun)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Hex{Hex(rowPrefix + other + "#case")}; !cmp.Equal(rows, want) {
		t.Errorf("case rows after the cleanup = %q, want %q", rows, want)
	}
	tables, err := caseTables(ctx, target, everyRun)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{tablePrefix + other + "-t1"}; !cmp.Equal(tables, want) {
		t.Errorf("case tables after the cleanup = %v, want %v", tables, want)
	}
	views, err := caseViews(ctx, target, everyRun)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{tablePrefix + other + "-v1"}; !cmp.Equal(views, want) {
		t.Errorf("case views after the cleanup = %v, want %v", views, want)
	}
	stream, err := target.Data.ReadRows(ctx, &btpb.ReadRowsRequest{
		TableName: target.tablePath(parityTable), Rows: &btpb.RowSet{RowKeys: [][]byte{[]byte("not a case row")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	if err := recvAll(stream, func(resp *btpb.ReadRowsResponse) { kept = kept || len(resp.Chunks) > 0 }); err != nil {
		t.Fatal(err)
	}
	if !kept {
		t.Error("the cleanup deleted a row that no case wrote")
	}
}
