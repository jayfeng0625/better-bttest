// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"context"
	"strings"
	"testing"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/testing/protocmp"
)

const testRunID = "0123456789ab"

func startGate(t *testing.T) (context.Context, Target) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	target, stop, err := StartGate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return ctx, target
}

func TestRunReadsTheMergedRow(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{Name: "merge", Calls: []Call{
		Mutate(AddToCell(Sum, Int(456))),
		Mutate(AddToCell(Sum, Int(123))),
		Read,
	}}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	want := []Result{
		{Call: "MutateRow", Status: Status{Code: codes.OK}},
		{Call: "MutateRow", Status: Status{Code: codes.OK}},
		{Call: "ReadRow", Status: Status{Code: codes.OK}, Cells: []Cell{{Column: "sum:c", TS: 1000, Value: BE(579)}}},
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
	}
}

func TestRunRecordsARejectedCallAndGoesOn(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{Name: "rejected", Calls: []Call{Mutate(SetCell(Sum, BE(456))), Read}}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || got[0].Status.Code != codes.InvalidArgument || got[0].Status.Message == "" {
		t.Fatalf("Run = %+v, want an InvalidArgument with a message, then the read", got)
	}
	if want := (Result{Call: "ReadRow", Status: Status{Code: codes.OK}}); !cmp.Equal(want, got[1]) {
		t.Errorf("read after the rejected write = %+v, want %+v", got[1], want)
	}
}

func TestRunFailsTheCaseWhenASetupCallFails(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{Name: "bad setup", Setup: []Call{Mutate(SetCell(Sum, BE(456)))}, Calls: []Call{Read}}

	if got, err := Run(ctx, target, testRunID, 1, c); err == nil {
		t.Fatalf("Run = %+v, want an error for the setup call", got)
	}
}

// Production rejects the whole batch when one entry is invalid, and each entry's message names its row.
func TestRunRecordsEachMutateRowsEntryInOrder(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{Name: "batch", Calls: []Call{
		MutateRows{Entries: []Entry{
			{Row: "first", Mutations: []*btpb.Mutation{SetCell(Sum, BE(456))}},
			{Row: "second", Mutations: []*btpb.Mutation{AddToCell(Sum, Int(456))}},
		}},
		ReadRow{Row: "second"},
	}}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || got[0].Status.Code != codes.OK || len(got[0].Entries) != 2 {
		t.Fatalf("Run = %+v, want an OK MutateRows with two entries, then the read", got)
	}
	for i, row := range []string{"#batch#first", "#batch#second"} {
		if e := got[0].Entries[i]; e.Code != codes.InvalidArgument || !strings.Contains(e.Message, row) {
			t.Errorf("entry %d = %+v, want InvalidArgument naming %s", i, e, row)
		}
	}
	if got[1].Cells != nil {
		t.Errorf("second row = %+v, want no cells", got[1].Cells)
	}
}

func TestRunRecordsWhetherCheckAndMutatePredicateMatched(t *testing.T) {
	ctx, target := startGate(t)
	branches := CheckAndMutate{True: []*btpb.Mutation{SetCell(Plain, BE(1))}, False: []*btpb.Mutation{SetCell(Plain, BE(2))}}
	c := Case{Name: "branch", Calls: []Call{branches, Read, branches, Read}}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	no, yes := false, true
	want := []Result{
		{Call: "CheckAndMutate", Status: Status{Code: codes.OK}, Matched: &no},
		{Call: "ReadRow", Status: Status{Code: codes.OK}, Cells: []Cell{{Column: "plain:c", TS: 1000, Value: BE(2)}}},
		{Call: "CheckAndMutate", Status: Status{Code: codes.OK}, Matched: &yes},
		{Call: "ReadRow", Status: Status{Code: codes.OK}, Cells: []Cell{{Column: "plain:c", TS: 1000, Value: BE(1)}}},
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
	}
}

// ReadModifyWriteRow writes a new cell at the server's clock, so the plain family keeps both.
func TestRunAppliesReadModifyWrite(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{
		Name:  "increment",
		Setup: []Call{Mutate(SetCell(Plain, BE(5)))},
		Calls: []Call{ReadModifyWrite{Rules: []*btpb.ReadModifyWriteRule{Increment(Plain)}}, Read},
	}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || !cmp.Equal(got[0], Result{Call: "ReadModifyWrite", Status: Status{Code: codes.OK}}) || len(got[1].Cells) != 2 {
		t.Fatalf("Run = %+v, want an OK ReadModifyWrite, then a read of two cells", got)
	}
	if latest := got[1].Cells[0]; latest.TS < serverClockMicros || !cmp.Equal(latest.Value, Hex(BE(6))) {
		t.Errorf("latest cell = %+v, want 6 at the server's clock", latest)
	}
}

func TestRunDeletesTheTableItCreated(t *testing.T) {
	ctx, target := startGate(t)
	schema := Delimited("#", "a", "b")
	c := Case{Name: "table", Calls: []Call{CreateTable{Schema: schema}, GetTable{}}}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	want := []Result{
		{Call: "CreateTable", Status: Status{Code: codes.OK}},
		{Call: "GetTable", Status: Status{Code: codes.OK}, Table: &TableView{RowKeySchema: schema}},
	}
	if d := cmp.Diff(want, got, protocmp.Transform()); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
	}
	if tables, err := caseTables(ctx, target); err != nil || len(tables) > 0 {
		t.Errorf("case tables after the case = %v, %v, want none", tables, err)
	}
}

func TestRunDeletesAProtectedTable(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{
		Name:  "protected",
		Setup: []Call{CreateTable{}, SetDeletionProtection{On: true}},
		Calls: []Call{GetTable{}},
	}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Table == nil || !got[0].Table.DeletionProtection {
		t.Fatalf("Run = %+v, want a GetTable of a protected table", got)
	}
	if tables, err := caseTables(ctx, target); err != nil || len(tables) > 0 {
		t.Errorf("case tables after the case = %v, %v, want none", tables, err)
	}
}

func TestRunSetsAndClearsTheRowKeySchema(t *testing.T) {
	ctx, target := startGate(t)
	schema := Delimited("#", "a")
	c := Case{
		Name:  "row key schema",
		Setup: []Call{CreateTable{}},
		Calls: []Call{
			SetRowKeySchema{Schema: schema}, GetTable{},
			SetRowKeySchema{IgnoreWarnings: true}, GetTable{},
		},
	}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	want := []Result{
		{Call: "SetRowKeySchema", Status: Status{Code: codes.OK}},
		{Call: "GetTable", Status: Status{Code: codes.OK}, Table: &TableView{RowKeySchema: schema}},
		{Call: "SetRowKeySchema", Status: Status{Code: codes.OK}},
		{Call: "GetTable", Status: Status{Code: codes.OK}, Table: &TableView{}},
	}
	if d := cmp.Diff(want, got, protocmp.Transform()); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
	}
}

func TestRunKeepsOnlyTheCodeOfAMissingTable(t *testing.T) {
	ctx, target := startGate(t)
	c := Case{Name: "missing", Calls: []Call{GetTable{}}}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	if want := []Result{{Call: "GetTable", Status: Status{Code: codes.NotFound}}}; !cmp.Equal(want, got) {
		t.Errorf("Run = %+v, want %+v", got, want)
	}
}

func TestRunWritesExactKeysOnACaseTable(t *testing.T) {
	ctx, target := startGate(t)
	write := func(key Row) MutateRow {
		return MutateRow{CaseTable: true, Row: key, Mutations: []*btpb.Mutation{SetCell("cf", []byte("v"))}}
	}
	c := Case{
		Name:  "keys",
		Setup: []Call{CreateTable{}},
		Calls: []Call{write("b"), write("a\xff"), ReadRow{CaseTable: true, Row: "b"}, ReadRowKeys{}},
	}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	ok := Status{Code: codes.OK}
	want := []Result{
		{Call: "MutateRow", Status: ok},
		{Call: "MutateRow", Status: ok},
		{Call: "ReadRow", Status: ok, Cells: []Cell{{Column: "cf:c", TS: 1000, Value: []byte("v")}}},
		{Call: "ReadRowKeys", Status: ok, Keys: []Hex{Hex("a\xff"), Hex("b")}},
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
	}
}
