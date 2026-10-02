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

// Production reads a missing input, or an input with no kind, as NULL. It adds a NULL AddToCell input as 0.
func TestAddToCellWithNullInputAddsZero(t *testing.T) {
	for name, input := range nullInputs() {
		t.Run(name, func(t *testing.T) {
			s, tbl := newAggregateTable(t, minAggregate())
			mutate(t, s, tbl, addToCell(456))
			m := addToCell(0)
			m.GetAddToCell().Input = input
			mutate(t, s, tbl, m)
			want := []aggregateCell{{aggregateTS, 0}}
			if got := readCells(t, s, tbl, nil); !slices.Equal(got, want) {
				t.Errorf("got cells %v, want %v", got, want)
			}
		})
	}
}

func TestMergeToCellWithNullInputHasNoEffect(t *testing.T) {
	for name, input := range nullInputs() {
		t.Run(name, func(t *testing.T) {
			s, tbl := newAggregateTable(t, minAggregate())
			null := mergeToCell(0)
			null.GetMergeToCell().Input = input
			mutate(t, s, tbl, null)
			if got := readCells(t, s, tbl, nil); len(got) != 0 {
				t.Errorf("on an empty cell: got cells %v, want none", got)
			}
			mutate(t, s, tbl, mergeToCell(456))
			mutate(t, s, tbl, null)
			want := []aggregateCell{{aggregateTS, 456}}
			if got := readCells(t, s, tbl, nil); !slices.Equal(got, want) {
				t.Errorf("on 456: got cells %v, want %v", got, want)
			}
		})
	}
}

func nullInputs() map[string]*btpb.Value {
	return map[string]*btpb.Value{"no input": nil, "an input with no kind": {}}
}

// Production allows every delete on an aggregate family.
func TestAggregateFamilyAcceptsDeletes(t *testing.T) {
	column := []byte(aggregateColumn)
	tests := []struct {
		name      string
		mutation  *btpb.Mutation
		wantPlain []aggregateCell
	}{
		{"DeleteFromColumn", &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromColumn_{DeleteFromColumn: &btpb.Mutation_DeleteFromColumn{
			FamilyName: aggregateFamily, ColumnQualifier: column,
		}}}, []aggregateCell{{aggregateTS, 456}}},
		{"DeleteFromColumn with a time range", &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromColumn_{DeleteFromColumn: &btpb.Mutation_DeleteFromColumn{
			FamilyName: aggregateFamily, ColumnQualifier: column,
			TimeRange: &btpb.TimestampRange{StartTimestampMicros: aggregateTS, EndTimestampMicros: laterTS},
		}}}, []aggregateCell{{aggregateTS, 456}}},
		{"DeleteFromFamily", &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromFamily_{DeleteFromFamily: &btpb.Mutation_DeleteFromFamily{
			FamilyName: aggregateFamily,
		}}}, []aggregateCell{{aggregateTS, 456}}},
		{"DeleteFromRow", &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromRow_{DeleteFromRow: &btpb.Mutation_DeleteFromRow{}}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, tbl := newAggregateTable(t, minAggregate())
			mutate(t, s, tbl, addToCell(456))
			mutate(t, s, tbl, setCellIn(plainFamily, 456))
			mutate(t, s, tbl, tc.mutation)
			if got := readCells(t, s, tbl, inFamily(aggregateFamily)); len(got) != 0 {
				t.Errorf("%s: got cells %v, want none", aggregateFamily, got)
			}
			if got := readCells(t, s, tbl, inFamily(plainFamily)); !slices.Equal(got, tc.wantPlain) {
				t.Errorf("%s: got cells %v, want %v", plainFamily, got, tc.wantPlain)
			}
		})
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
		name    string
		req     *btpb.CheckAndMutateRowRequest
		wantErr bool
		wantAgg []aggregateCell
	}{
		{
			name:    "SetCell on an aggregate family in the applied branch",
			req:     &btpb.CheckAndMutateRowRequest{TrueMutations: []*btpb.Mutation{setCellIn(aggregateFamily, 789)}},
			wantErr: true,
		},
		{
			name: "SetCell on an aggregate family in the branch it skips",
			req: &btpb.CheckAndMutateRowRequest{
				TrueMutations:  []*btpb.Mutation{addToCell(123)},
				FalseMutations: []*btpb.Mutation{setCellIn(aggregateFamily, 789)},
			},
			wantAgg: []aggregateCell{{aggregateTS, 123}},
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
			if got := readCells(t, s, tbl, inFamily(aggregateFamily)); !slices.Equal(got, tc.wantAgg) {
				t.Errorf("%s: got cells %v, want %v", aggregateFamily, got, tc.wantAgg)
			}
			wantPlain := []aggregateCell{{aggregateTS, 456}}
			if got := readCells(t, s, tbl, inFamily(plainFamily)); !slices.Equal(got, wantPlain) {
				t.Errorf("%s: got cells %v, want %v", plainFamily, got, wantPlain)
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

// Production checks each mutation's input kind for the whole request before it writes anything.
func TestMutateRowRejectsWrongInputKind(t *testing.T) {
	addBytes := addToCell(0)
	addBytes.GetAddToCell().Input = &btpb.Value{Kind: &btpb.Value_BytesValue{BytesValue: binary.BigEndian.AppendUint64(nil, 456)}}
	mergeInt := mergeToCell(0)
	mergeInt.GetMergeToCell().Input = &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: 456}}
	mergeRaw := mergeToCell(0)
	mergeRaw.GetMergeToCell().Input = &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: binary.BigEndian.AppendUint64(nil, 456)}}
	tests := []struct {
		name      string
		mutations []*btpb.Mutation
		want      string
	}{
		{
			name:      "AddToCell with a bytes input",
			mutations: []*btpb.Mutation{addBytes},
			want:      "Error in field 'Mutation list' : Error in element #0 : Error in field 'input' : must use `int_value`",
		},
		{
			name:      "MergeToCell with an int input",
			mutations: []*btpb.Mutation{mergeInt},
			want:      "Error in field 'Mutation list' : Error in element #0 : Error in field 'input' : must use `bytes_value`",
		},
		{
			name:      "MergeToCell with a raw input",
			mutations: []*btpb.Mutation{mergeRaw},
			want:      "Error in field 'Mutation list' : Error in element #0 : Error in field 'input' : must use `bytes_value`",
		},
		{
			name:      "SetCell on a plain family, then AddToCell with a bytes input",
			mutations: []*btpb.Mutation{setCellIn(plainFamily, 456), addBytes},
			want:      "Error in field 'Mutation list' : Error in element #1 : Error in field 'input' : must use `int_value`",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, tbl := newAggregateTable(t, minAggregate())
			_, err := s.MutateRow(context.Background(), &btpb.MutateRowRequest{
				TableName: tbl,
				RowKey:    []byte("row"),
				Mutations: tc.mutations,
			})
			wantStatus(t, err, codes.InvalidArgument, tc.want)
			if got := readCells(t, s, tbl, nil); len(got) != 0 {
				t.Errorf("got cells %v, want none", got)
			}
		})
	}
}

// Production fails the whole MutateRows RPC on a wrong input kind, before it writes any entry.
func TestMutateRowsRejectsWrongInputKind(t *testing.T) {
	s, tbl := newAggregateTable(t, minAggregate())
	addBytes := addToCell(0)
	addBytes.GetAddToCell().Input = &btpb.Value{Kind: &btpb.Value_BytesValue{BytesValue: binary.BigEndian.AppendUint64(nil, 456)}}
	err := s.MutateRows(&btpb.MutateRowsRequest{TableName: tbl, Entries: []*btpb.MutateRowsRequest_Entry{
		{RowKey: []byte("good"), Mutations: []*btpb.Mutation{setCellIn(plainFamily, 456)}},
		{RowKey: []byte("bad"), Mutations: []*btpb.Mutation{setCellIn(plainFamily, 456), addBytes}},
	}}, &mutateRowsRecorder{})
	wantStatus(t, err, codes.InvalidArgument, "Error in field 'Entry list' : Error in element #1 : Error in field 'Mutation list' : Error in element #1 : Error in field 'input' : must use `int_value`")
	if got := readCells(t, s, tbl, nil); len(got) != 0 {
		t.Errorf("got cells %v, want none", got)
	}
}

// Production checks input kinds in both CheckAndMutateRow branches, including the one it skips.
func TestCheckAndMutateRowRejectsWrongInputKindInEitherBranch(t *testing.T) {
	addBytes := addToCell(0)
	addBytes.GetAddToCell().Input = &btpb.Value{Kind: &btpb.Value_BytesValue{BytesValue: binary.BigEndian.AppendUint64(nil, 456)}}
	tests := []struct {
		name string
		req  *btpb.CheckAndMutateRowRequest
		want string
	}{
		{
			name: "in the applied branch",
			req:  &btpb.CheckAndMutateRowRequest{TrueMutations: []*btpb.Mutation{setCellIn(plainFamily, 789), addBytes}},
			want: "Error in field 'true mutation list' : Error in element #1 : Error in field 'input' : must use `int_value`",
		},
		{
			name: "in the branch it skips",
			req: &btpb.CheckAndMutateRowRequest{
				TrueMutations:  []*btpb.Mutation{setCellIn(plainFamily, 789)},
				FalseMutations: []*btpb.Mutation{addBytes},
			},
			want: "Error in field 'false mutation list' : Error in element #0 : Error in field 'input' : must use `int_value`",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, tbl := newAggregateTable(t, minAggregate())
			mutate(t, s, tbl, setCellIn(plainFamily, 456))
			tc.req.TableName, tc.req.RowKey = tbl, []byte("row")
			_, err := s.CheckAndMutateRow(context.Background(), tc.req)
			wantStatus(t, err, codes.InvalidArgument, tc.want)
			want := []aggregateCell{{aggregateTS, 456}}
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

func inFamily(family string) *btpb.RowFilter {
	return &btpb.RowFilter{Filter: &btpb.RowFilter_FamilyNameRegexFilter{FamilyNameRegexFilter: family}}
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
