// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/testing/protocmp"
)

const testRunID = "0123456789ab"

// The families of a table that CreateTable makes with no Families.
var oneVersion = map[string]*adminpb.ColumnFamily{"cf": {GcRule: &adminpb.GcRule{Rule: &adminpb.GcRule_MaxNumVersions{MaxNumVersions: 1}}}}

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
		{Call: "GetTable", Status: Status{Code: codes.OK}, Table: &TableView{ColumnFamilies: oneVersion, RowKeySchema: schema}},
	}
	if d := cmp.Diff(want, got, protocmp.Transform()); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
	}
	if tables, err := caseTables(ctx, target, func(string) bool { return true }); err != nil || len(tables) > 0 {
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
	if tables, err := caseTables(ctx, target, func(string) bool { return true }); err != nil || len(tables) > 0 {
		t.Errorf("case tables after the case = %v, %v, want none", tables, err)
	}
}

// A call that ends the case's context, as a case that runs past its deadline does.
type endCase struct{ cancel context.CancelFunc }

func (c endCase) run(context.Context, *runner) (Result, error) {
	c.cancel()
	return Result{}, nil
}

func TestRunDeletesItsTableAfterTheCaseContextEnds(t *testing.T) {
	ctx, target := startGate(t)
	caseCtx, cancel := context.WithCancel(ctx)
	c := Case{Name: "overrun", Setup: []Call{CreateTable{}}, Calls: []Call{endCase{cancel}}}

	if _, err := Run(caseCtx, target, testRunID, 1, c); err != nil {
		t.Fatal(err)
	}

	if tables, err := caseTables(ctx, target, func(string) bool { return true }); err != nil || len(tables) > 0 {
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
		{Call: "GetTable", Status: Status{Code: codes.OK}, Table: &TableView{ColumnFamilies: oneVersion, RowKeySchema: schema}},
		{Call: "SetRowKeySchema", Status: Status{Code: codes.OK}},
		{Call: "GetTable", Status: Status{Code: codes.OK}, Table: &TableView{ColumnFamilies: oneVersion}},
	}
	if d := cmp.Diff(want, got, protocmp.Transform()); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
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
		Calls: []Call{write("b"), write("a\xff"), ReadRowKeys{}},
	}

	got, err := Run(ctx, target, testRunID, 1, c)
	if err != nil {
		t.Fatal(err)
	}

	ok := Status{Code: codes.OK}
	want := []Result{
		{Call: "MutateRow", Status: ok},
		{Call: "MutateRow", Status: ok},
		{Call: "ReadRowKeys", Status: ok, Keys: []Hex{Hex("a\xff"), Hex("b")}},
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("Run (-want +got):\n%s", d)
	}
}

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
	for i, c := range slices.Concat(SQLCases(), TotalsCases()) {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if _, err := Run(ctx, target, testRunID, i+1, c); err != nil {
				t.Fatal(err)
			}
		})
	}
}
