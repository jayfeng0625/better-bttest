// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"context"
	"encoding/binary"
	"slices"
	"testing"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
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

const (
	aggregateFamily = "agg"
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

func newAggregateTable(t *testing.T, aggregator *btapb.Type_Aggregate) (*server, string) {
	t.Helper()
	s := &server{tables: make(map[string]*table)}
	tbl, err := s.CreateTable(context.Background(), &btapb.CreateTableRequest{
		Parent:  "cluster",
		TableId: "t",
		Table: &btapb.Table{ColumnFamilies: map[string]*btapb.ColumnFamily{
			aggregateFamily: {ValueType: &btapb.Type{Kind: &btapb.Type_AggregateType{AggregateType: aggregator}}},
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
