// SPDX-License-Identifier: Apache-2.0

//go:build googlesql

package parity

import (
	"context"
	"slices"
	"testing"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/testing/protocmp"
)

// A view row has one family, default, with an empty-qualifier cell that holds no value, then a cell per stored
// column, all at timestamp 0.
func viewRow(key string, cells ...Cell) ViewRow {
	return ViewRow{Key: Hex(key), Cells: append([]Cell{{Column: "default:"}}, cells...)}
}

func TestRunCreatesReadsAndDeletesTheCaseView(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{
		Name:  "view",
		Setup: twoRows,
		Calls: slices.Concat(
			[]Call{CreateView{Query: "SELECT _key, COUNT(*) AS n FROM `{table}` GROUP BY _key"}, GetView{}, ReadView{}},
			Query("SELECT * FROM `{view}`"),
			[]Call{DeleteView{}, GetView{}},
		),
	}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	ok := Status{Code: codes.OK}
	want := []Result{
		{Call: "CreateView", Status: ok},
		{Call: "GetView", Status: ok, View: &ViewInfo{Query: "SELECT _key, COUNT(*) AS n FROM `better-bttest-parity-0123456789ab-t1` GROUP BY _key"}},
		{Call: "ReadView", Status: ok, ViewRows: []ViewRow{
			viewRow("a", Cell{Column: "default:n", Value: BE(1)}),
			viewRow("b", Cell{Column: "default:n", Value: BE(1)}),
		}},
		{Call: "PrepareQuery", Status: ok, Columns: []Column{{Name: "_key", Type: BytesType}, {Name: "n", Type: Int64Type}}},
		{
			Call: "ExecuteQuery", Status: ok,
			Rows:     [][]*btpb.Value{{Bytes([]byte("a")), Int(1)}, {Bytes([]byte("b")), Int(1)}},
			Messages: []string{"batch rows=2 reset checksum", "token"},
		},
		{Call: "DeleteView", Status: ok},
		{Call: "GetView", Status: Status{Code: codes.NotFound, Message: "Failed to read: projects/{parity}/instances/parity/materializedViews/better-bttest-parity-0123456789ab-v1"}},
	}
	if d := cmp.Diff(want, got, protocmp.Transform()); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
	}
}

// The run deletes a protected view before its table, since production refuses to delete a table while a view reads
// it.
func TestRunDeletesAProtectedViewBeforeItsTable(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{
		Name:  "protected view",
		Setup: slices.Concat(twoRows, []Call{CreateView{Query: "SELECT _key AS k FROM `{table}` ORDER BY k"}, SetViewDeletionProtection{On: true}}),
		Calls: []Call{GetView{}},
	}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	want := []Result{{Call: "GetView", Status: Status{Code: codes.OK}, View: &ViewInfo{Query: "SELECT _key AS k FROM `better-bttest-parity-0123456789ab-t1` ORDER BY k", DeletionProtection: true}}}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
	}
	everyRun := func(string) bool { return true }
	if views, err := caseViews(ctx, target, everyRun); err != nil || len(views) > 0 {
		t.Errorf("case views after the case = %v, %v, want none", views, err)
	}
	if tables, err := caseTables(ctx, target, everyRun); err != nil || len(tables) > 0 {
		t.Errorf("case tables after the case = %v, %v, want none", tables, err)
	}
}

func TestRunNamesTheCaseViewInAQueryOnNoView(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{Name: "no view", Calls: Query("SELECT _key FROM `{view}`")}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	want := Status{Code: codes.InvalidArgument, Message: "Table not found: `better-bttest-parity-<run>-v1` [at 1:18]"}
	if d := cmp.Diff(want, Normalize(target.Instance, testRunID, got)[0].Status); d != "" {
		t.Errorf("PrepareQuery status (-want +got):\n%s", d)
	}
}

// Each view case's Setup succeeds on the gate.
func TestViewCasesRunOnTheGate(t *testing.T) {
	_, target := startGate(t)
	for i, c := range ViewCases() {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), deadline(c))
			defer cancel()
			if _, err := Run(ctx, target, testRunID, i+1, c); err != nil {
				t.Fatal(err)
			}
		})
	}
}
