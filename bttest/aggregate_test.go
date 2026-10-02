// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"context"
	"encoding/binary"
	"slices"
	"testing"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	statpb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAggregateMerges(t *testing.T) {
	tests := []struct {
		name       string
		aggregator *btapb.Type_Aggregate
		write      func(int64) *btpb.Mutation
		inputs     []int64
		want       int64
	}{
		{
			name:       "MIN over AddToCell keeps the minimum",
			aggregator: minAggregate(),
			write:      addToCell,
			inputs:     []int64{456, 123, 789},
			want:       123,
		},
		{
			name:       "MIN over MergeToCell keeps the minimum",
			aggregator: minAggregate(),
			write:      mergeToCell,
			inputs:     []int64{456, 123, 789},
			want:       123,
		},
		{
			name:       "MAX over AddToCell keeps the maximum",
			aggregator: maxAggregate(),
			write:      addToCell,
			inputs:     []int64{456, 789, 123},
			want:       789,
		},
		{
			name:       "MAX over MergeToCell keeps the maximum",
			aggregator: maxAggregate(),
			write:      mergeToCell,
			inputs:     []int64{456, 789, 123},
			want:       789,
		},
		{
			name:       "MIN compares negative values as signed",
			aggregator: minAggregate(),
			write:      addToCell,
			inputs:     []int64{-3, 5},
			want:       -3,
		},
		{
			name:       "MIN over MergeToCell compares negative values as signed",
			aggregator: minAggregate(),
			write:      mergeToCell,
			inputs:     []int64{5, -3},
			want:       -3,
		},
		{
			name:       "MAX compares negative values as signed",
			aggregator: maxAggregate(),
			write:      mergeToCell,
			inputs:     []int64{5, -3},
			want:       5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, tbl := newAggregateTable(t, tc.aggregator)
			for _, v := range tc.inputs {
				mutate(t, s, tbl, tc.write(v))
			}
			want := []aggregateCell{{aggregateTS, tc.want}}
			if got := readCells(t, s, tbl, nil); !slices.Equal(got, want) {
				t.Errorf("got cells %v, want %v", got, want)
			}
		})
	}
}

// Bigtable merges only cells at the same timestamp.
func TestAggregateWriteAtNewTimestampStartsCell(t *testing.T) {
	s, tbl := newAggregateTable(t, minAggregate())
	mutate(t, s, tbl, addToCellAt(aggregateTS, 123))
	mutate(t, s, tbl, addToCellAt(laterTS, 789))
	want := []aggregateCell{{laterTS, 789}, {aggregateTS, 123}}
	if got := readCells(t, s, tbl, nil); !slices.Equal(got, want) {
		t.Errorf("got cells %v, want %v", got, want)
	}
	latest := &btpb.RowFilter{Filter: &btpb.RowFilter_CellsPerColumnLimitFilter{CellsPerColumnLimitFilter: 1}}
	want = []aggregateCell{{laterTS, 789}}
	if got := readCells(t, s, tbl, latest); !slices.Equal(got, want) {
		t.Errorf("latest version: got cells %v, want %v", got, want)
	}
}

// Production adds a NULL AddToCell input as 0.
func TestAddToCellWithNoInputAddsZero(t *testing.T) {
	s, tbl := newAggregateTable(t, minAggregate())
	mutate(t, s, tbl, addToCell(456))
	m := addToCell(0)
	m.GetAddToCell().Input = nil
	mutate(t, s, tbl, m)
	want := []aggregateCell{{aggregateTS, 0}}
	if got := readCells(t, s, tbl, nil); !slices.Equal(got, want) {
		t.Errorf("got cells %v, want %v", got, want)
	}
}

// Production ignores a NULL MergeToCell input.
func TestMergeToCellWithNoInputHasNoEffect(t *testing.T) {
	s, tbl := newAggregateTable(t, minAggregate())
	noInput := mergeToCell(0)
	noInput.GetMergeToCell().Input = nil
	mutate(t, s, tbl, noInput)
	if got := readCells(t, s, tbl, nil); len(got) != 0 {
		t.Errorf("on an empty cell: got cells %v, want none", got)
	}
	mutate(t, s, tbl, mergeToCell(456))
	mutate(t, s, tbl, noInput)
	want := []aggregateCell{{aggregateTS, 456}}
	if got := readCells(t, s, tbl, nil); !slices.Equal(got, want) {
		t.Errorf("on 456: got cells %v, want %v", got, want)
	}
}

// Production accepts a MergeToCell input only as bytes_value.
func TestMergeToCellRejectsRawValueInput(t *testing.T) {
	s, tbl := newAggregateTable(t, minAggregate())
	m := mergeToCell(123)
	input := m.GetMergeToCell().Input
	input.Kind = &btpb.Value_RawValue{RawValue: input.GetBytesValue()}
	_, err := s.MutateRow(context.Background(), &btpb.MutateRowRequest{
		TableName: tbl,
		RowKey:    []byte("row"),
		Mutations: []*btpb.Mutation{m},
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("got code %v (%v), want %v", got, err, codes.InvalidArgument)
	}
	if got := readCells(t, s, tbl, nil); len(got) != 0 {
		t.Errorf("got cells %v, want none", got)
	}
}

// Production rejects a mutation that does not fit its family's type and writes nothing.
func TestMutateRowRejectsFamilyTypeMismatch(t *testing.T) {
	addToPlain := addToCell(456)
	addToPlain.GetAddToCell().FamilyName = plainFamily
	mergeToPlain := mergeToCell(456)
	mergeToPlain.GetMergeToCell().FamilyName = plainFamily
	tests := []struct {
		name      string
		mutations []*btpb.Mutation
	}{
		{"SetCell on an aggregate family", []*btpb.Mutation{setCellIn(aggregateFamily, 456)}},
		{"AddToCell on a plain family", []*btpb.Mutation{addToPlain}},
		{"MergeToCell on a plain family", []*btpb.Mutation{mergeToPlain}},
		{"AddToCell, then SetCell on an aggregate family", []*btpb.Mutation{addToCell(456), setCellIn(aggregateFamily, 456)}},
		{"SetCell on a plain family, then on an aggregate family", []*btpb.Mutation{setCellIn(plainFamily, 456), setCellIn(aggregateFamily, 456)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, tbl := newAggregateTable(t, minAggregate())
			_, err := s.MutateRow(context.Background(), &btpb.MutateRowRequest{
				TableName: tbl,
				RowKey:    []byte("row"),
				Mutations: tc.mutations,
			})
			wantStatus(t, err, codes.InvalidArgument, familyTypeMismatchMessage(tbl, "row"))
			if got := readCells(t, s, tbl, nil); len(got) != 0 {
				t.Errorf("got cells %v, want none", got)
			}
		})
	}
}

// Production fails every entry of a MutateRows batch, each with its own row, when any entry has a family type
// mismatch. The RPC returns OK and writes nothing.
func TestMutateRowsFailsBatchOnFamilyTypeMismatch(t *testing.T) {
	addToPlain := addToCell(456)
	addToPlain.GetAddToCell().FamilyName = plainFamily
	tests := []struct {
		name    string
		entries []*btpb.MutateRowsRequest_Entry
	}{
		{"SetCell on an aggregate family, then a valid entry", []*btpb.MutateRowsRequest_Entry{
			{RowKey: []byte("bad"), Mutations: []*btpb.Mutation{setCellIn(aggregateFamily, 456)}},
			{RowKey: []byte("good"), Mutations: []*btpb.Mutation{addToCell(456)}},
		}},
		{"a valid entry, then SetCell on an aggregate family", []*btpb.MutateRowsRequest_Entry{
			{RowKey: []byte("good"), Mutations: []*btpb.Mutation{setCellIn(plainFamily, 456)}},
			{RowKey: []byte("bad"), Mutations: []*btpb.Mutation{setCellIn(aggregateFamily, 456)}},
		}},
		{"AddToCell on a plain family, then a valid entry", []*btpb.MutateRowsRequest_Entry{
			{RowKey: []byte("bad"), Mutations: []*btpb.Mutation{addToPlain}},
			{RowKey: []byte("good"), Mutations: []*btpb.Mutation{setCellIn(plainFamily, 456)}},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, tbl := newAggregateTable(t, minAggregate())
			stream := &mutateRowsRecorder{}
			if err := s.MutateRows(&btpb.MutateRowsRequest{TableName: tbl, Entries: tc.entries}, stream); err != nil {
				t.Fatal(err)
			}
			for i, entry := range tc.entries {
				got := stream.status(i)
				want := familyTypeMismatchMessage(tbl, string(entry.RowKey))
				if got.GetCode() != int32(codes.InvalidArgument) || got.GetMessage() != want {
					t.Errorf("entry %d: got status %v, want code %d and message %q", i, got, codes.InvalidArgument, want)
				}
			}
			if got := readCells(t, s, tbl, nil); len(got) != 0 {
				t.Errorf("got cells %v, want none", got)
			}
		})
	}
}

// Production checks family types only in the CheckAndMutateRow branch it applies. With no predicate, a row
// with cells takes the true branch.
func TestCheckAndMutateRowChecksFamilyTypesInAppliedBranch(t *testing.T) {
	tests := []struct {
		name      string
		req       *btpb.CheckAndMutateRowRequest
		wantErr   bool
		wantCells []aggregateCell
	}{
		{
			name:      "SetCell on an aggregate family in the applied branch",
			req:       &btpb.CheckAndMutateRowRequest{TrueMutations: []*btpb.Mutation{setCellIn(aggregateFamily, 789)}},
			wantErr:   true,
			wantCells: []aggregateCell{{aggregateTS, 456}},
		},
		{
			name: "SetCell on an aggregate family in the branch it skips",
			req: &btpb.CheckAndMutateRowRequest{
				TrueMutations:  []*btpb.Mutation{addToCell(123)},
				FalseMutations: []*btpb.Mutation{setCellIn(aggregateFamily, 789)},
			},
			wantCells: []aggregateCell{{aggregateTS, 123}, {aggregateTS, 456}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, tbl := newAggregateTable(t, minAggregate())
			mutate(t, s, tbl, setCellIn(plainFamily, 456))
			tc.req.TableName, tc.req.RowKey = tbl, []byte("row")
			res, err := s.CheckAndMutateRow(context.Background(), tc.req)
			if tc.wantErr {
				wantStatus(t, err, codes.InvalidArgument, familyTypeMismatchMessage(tbl, "row"))
			} else if err != nil || !res.PredicateMatched {
				t.Errorf("got response %v and error %v, want a matched predicate", res, err)
			}
			if got := readCells(t, s, tbl, nil); !slices.Equal(got, tc.wantCells) {
				t.Errorf("got cells %v, want %v", got, tc.wantCells)
			}
		})
	}
}

// Production rejects a ReadModifyWriteRow rule on an aggregate family and leaves the row unchanged.
func TestReadModifyWriteRowRejectsAggregateFamily(t *testing.T) {
	increment := func(family string) *btpb.ReadModifyWriteRule {
		return &btpb.ReadModifyWriteRule{FamilyName: family, ColumnQualifier: []byte(aggregateColumn), Rule: &btpb.ReadModifyWriteRule_IncrementAmount{IncrementAmount: 1}}
	}
	appendTo := func(family string) *btpb.ReadModifyWriteRule {
		return &btpb.ReadModifyWriteRule{FamilyName: family, ColumnQualifier: []byte(aggregateColumn), Rule: &btpb.ReadModifyWriteRule_AppendValue{AppendValue: []byte("x")}}
	}
	tests := []struct {
		name  string
		seed  *btpb.Mutation
		rules []*btpb.ReadModifyWriteRule
	}{
		{"increment", addToCell(456), []*btpb.ReadModifyWriteRule{increment(aggregateFamily)}},
		{"append", addToCell(456), []*btpb.ReadModifyWriteRule{appendTo(aggregateFamily)}},
		{"increment on an empty cell", nil, []*btpb.ReadModifyWriteRule{increment(aggregateFamily)}},
		{"increment on a plain family, then on an aggregate family", setCellIn(plainFamily, 456), []*btpb.ReadModifyWriteRule{increment(plainFamily), increment(aggregateFamily)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, tbl := newAggregateTable(t, minAggregate())
			var want []aggregateCell
			if tc.seed != nil {
				mutate(t, s, tbl, tc.seed)
				want = []aggregateCell{{aggregateTS, 456}}
			}
			_, err := s.ReadModifyWriteRow(context.Background(), &btpb.ReadModifyWriteRowRequest{TableName: tbl, RowKey: []byte("row"), Rules: tc.rules})
			wantStatus(t, err, codes.InvalidArgument, familyTypeMismatchMessage(tbl, "row"))
			if got := readCells(t, s, tbl, nil); !slices.Equal(got, want) {
				t.Errorf("got cells %v, want %v", got, want)
			}
		})
	}
}

const (
	aggregateFamily = "agg"
	plainFamily     = "plain"
	aggregateColumn = "col"
	aggregateTS     = microsPerMilli
	laterTS         = 2 * microsPerMilli
)

type aggregateCell struct {
	ts, value int64
}

func minAggregate() *btapb.Type_Aggregate {
	return &btapb.Type_Aggregate{
		InputType:  &btapb.Type{Kind: &btapb.Type_Int64Type{}},
		Aggregator: &btapb.Type_Aggregate_Min_{Min: &btapb.Type_Aggregate_Min{}},
	}
}

func maxAggregate() *btapb.Type_Aggregate {
	return &btapb.Type_Aggregate{
		InputType:  &btapb.Type{Kind: &btapb.Type_Int64Type{}},
		Aggregator: &btapb.Type_Aggregate_Max_{Max: &btapb.Type_Aggregate_Max{}},
	}
}

func mergeToCell(v int64) *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_MergeToCell_{MergeToCell: &btpb.Mutation_MergeToCell{
		FamilyName:      aggregateFamily,
		ColumnQualifier: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte(aggregateColumn)}},
		Timestamp:       &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: aggregateTS}},
		Input:           &btpb.Value{Kind: &btpb.Value_BytesValue{BytesValue: binary.BigEndian.AppendUint64(nil, uint64(v))}},
	}}}
}

func addToCell(v int64) *btpb.Mutation {
	return addToCellAt(aggregateTS, v)
}

func addToCellAt(ts, v int64) *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_AddToCell_{AddToCell: &btpb.Mutation_AddToCell{
		FamilyName:      aggregateFamily,
		ColumnQualifier: &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: []byte(aggregateColumn)}},
		Timestamp:       &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: ts}},
		Input:           &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: v}},
	}}}
}

func setCellIn(family string, v int64) *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
		FamilyName:      family,
		ColumnQualifier: []byte(aggregateColumn),
		TimestampMicros: aggregateTS,
		Value:           binary.BigEndian.AppendUint64(nil, uint64(v)),
	}}}
}

func newAggregateTable(t *testing.T, aggregator *btapb.Type_Aggregate) (*server, string) {
	t.Helper()
	s := &server{tables: make(map[string]*table)}
	tbl, err := s.CreateTable(context.Background(), &btapb.CreateTableRequest{
		Parent:  "cluster",
		TableId: "t",
		Table: &btapb.Table{ColumnFamilies: map[string]*btapb.ColumnFamily{
			aggregateFamily: {ValueType: &btapb.Type{Kind: &btapb.Type_AggregateType{AggregateType: aggregator}}},
			plainFamily:     {},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, tbl.Name
}

func mutate(t *testing.T, s *server, tbl string, m *btpb.Mutation) {
	t.Helper()
	_, err := s.MutateRow(context.Background(), &btpb.MutateRowRequest{
		TableName: tbl,
		RowKey:    []byte("row"),
		Mutations: []*btpb.Mutation{m},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readCells(t *testing.T, s *server, tbl string, filter *btpb.RowFilter) []aggregateCell {
	t.Helper()
	mock := &MockReadRowsServer{}
	if err := s.ReadRows(&btpb.ReadRowsRequest{TableName: tbl, Filter: filter}, mock); err != nil {
		t.Fatal(err)
	}
	var cells []aggregateCell
	for _, r := range mock.responses {
		for _, chunk := range r.Chunks {
			cells = append(cells, aggregateCell{chunk.TimestampMicros, int64(binary.BigEndian.Uint64(chunk.Value))})
		}
	}
	return cells
}

func familyTypeMismatchMessage(tbl, row string) string {
	return "Error while mutating the row '" + row + "' (" + tbl + ") : Column family type mismatch"
}

func wantStatus(t *testing.T, err error, code codes.Code, message string) {
	t.Helper()
	if got := status.Code(err); got != code {
		t.Errorf("got code %v, want %v", got, code)
	}
	if got := status.Convert(err).Message(); got != message {
		t.Errorf("got message %q, want %q", got, message)
	}
}

type mutateRowsRecorder struct {
	grpc.ServerStream
	entries []*btpb.MutateRowsResponse_Entry
}

func (r *mutateRowsRecorder) Send(res *btpb.MutateRowsResponse) error {
	r.entries = append(r.entries, res.Entries...)
	return nil
}

func (r *mutateRowsRecorder) status(index int) *statpb.Status {
	for _, e := range r.entries {
		if e.Index == int64(index) {
			return e.Status
		}
	}
	return nil
}
