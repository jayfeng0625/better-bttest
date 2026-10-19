// SPDX-License-Identifier: Apache-2.0

//go:build googlesql

package parity

import (
	"context"
	"testing"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/testing/protocmp"
)

// A case table with the rows a and b, each with cf:c = v.
var twoRows = []Call{
	CreateTable{},
	MutateRows{CaseTable: true, Entries: []Entry{
		{Row: "a", Mutations: Mutations(SetCell("cf", []byte("v")))},
		{Row: "b", Mutations: Mutations(SetCell("cf", []byte("v")))},
	}},
}

func TestRunDecodesAQueryOnTheCaseTable(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{Name: "query", Setup: twoRows, Calls: Query("SELECT _key, cf['c'] AS v FROM `{table}`")}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	ok := Status{Code: codes.OK}
	want := []Result{
		{Call: "PrepareQuery", Status: ok, Columns: []Column{{Name: "_key", Type: BytesType}, {Name: "v", Type: BytesType}}},
		{
			Call: "ExecuteQuery", Status: ok,
			Rows: [][]*btpb.Value{
				{Bytes([]byte("a")), Bytes([]byte("v"))},
				{Bytes([]byte("b")), Bytes([]byte("v"))},
			},
			Messages: []string{"batch rows=2 reset checksum", "token"},
		},
	}
	if d := cmp.Diff(want, got, protocmp.Transform()); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
	}
}

func TestRunRecordsTheEmptyBatchOfAQueryWithNoRows(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{Name: "no rows", Setup: twoRows, Calls: Query("SELECT _key FROM `{table}` WHERE _key = 'nope'")}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	ok := Status{Code: codes.OK}
	want := []Result{
		{Call: "PrepareQuery", Status: ok, Columns: []Column{{Name: "_key", Type: BytesType}}},
		{Call: "ExecuteQuery", Status: ok, Messages: []string{"batch rows=0 reset", "token"}},
	}
	if d := cmp.Diff(want, got, protocmp.Transform()); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
	}
}

func TestRunBindsAQueryParameter(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{
		Name:  "param",
		Setup: twoRows,
		Calls: Query("SELECT _key FROM `{table}` WHERE _key = @k", BytesParam("k", []byte("b"))),
	}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	ok := Status{Code: codes.OK}
	want := []Result{
		{Call: "PrepareQuery", Status: ok, Columns: []Column{{Name: "_key", Type: BytesType}}},
		{Call: "ExecuteQuery", Status: ok, Rows: [][]*btpb.Value{{Bytes([]byte("b"))}}, Messages: []string{"batch rows=1 reset checksum", "token"}},
	}
	if d := cmp.Diff(want, got, protocmp.Transform()); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
	}
}

// A failed PrepareQuery drops the query that an earlier one prepared.
func TestRunExecutesNothingAfterAFailedPrepare(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{
		Name:  "failed prepare",
		Setup: twoRows,
		Calls: []Call{PrepareQuery{SQL: "SELECT _key FROM `{table}`"}, PrepareQuery{SQL: "SELEC _key FROM `{table}`"}, ExecuteQuery{}},
	}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	want := Result{Call: "ExecuteQuery", Status: Status{Code: codes.FailedPrecondition, Message: "the case has no prepared query"}}
	if len(got) != 3 || got[1].Status.Code != codes.InvalidArgument {
		t.Fatalf("Run = %+v, want a prepared query, then an InvalidArgument, then the execute", got)
	}
	if d := cmp.Diff(want, got[2]); d != "" {
		t.Errorf("execute after the failed prepare (-want +got):\n%s", d)
	}
}

func TestRunNamesTheCaseTableInAQueryOnNoTable(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{Name: "no table", Calls: Query("SELECT _key FROM `{table}`")}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	want := Status{Code: codes.InvalidArgument, Message: "Table not found: `better-bttest-parity-<run>-t1` [at 1:18]"}
	if d := cmp.Diff(want, Normalize(target.Instance, testRunID, got)[0].Status); d != "" {
		t.Errorf("PrepareQuery status (-want +got):\n%s", d)
	}
}

// Each SQL case's Setup succeeds on the gate.
func TestSQLCasesRunOnTheGate(t *testing.T) {
	_, target := startGate(t)
	for i, c := range SQLCases() {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), deadline(c))
			defer cancel()
			if _, err := Run(ctx, target, testRunID, i+1, c); err != nil {
				t.Fatal(err)
			}
		})
	}
}
