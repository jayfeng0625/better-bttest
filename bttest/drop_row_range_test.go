// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"context"
	"slices"
	"testing"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
)

// Production rejects a DropRowRange whose deadline is under 2 minutes and keeps the rows, as a parity run recorded.
func TestDropRowRangeRejectsDeadlineUnderTwoMinutes(t *testing.T) {
	s := &server{tables: make(map[string]*table)}
	tbl, err := s.CreateTable(context.Background(), &btapb.CreateTableRequest{
		Parent:  "projects/p/instances/i",
		TableId: "t",
		Table:   &btapb.Table{ColumnFamilies: map[string]*btapb.ColumnFamily{"cf": {}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MutateRow(context.Background(), &btpb.MutateRowRequest{
		TableName: tbl.Name, RowKey: []byte("k"), Mutations: []*btpb.Mutation{setCellIn("cf", 456)},
	}); err != nil {
		t.Fatal(err)
	}
	drop := func(deadline time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		_, err := s.DropRowRange(ctx, &btapb.DropRowRangeRequest{
			Name: tbl.Name, Target: &btapb.DropRowRangeRequest_DeleteAllDataFromTable{DeleteAllDataFromTable: true},
		})
		return err
	}

	wantStatus(t, drop(time.Minute), codes.FailedPrecondition,
		"Insufficient deadline to delete a row range from projects/p/instances/i/tables/t. Please re-issue the request with a deadline at least 2m.")
	if got, want := rowKeys(t, s, tbl.Name), []string{"k"}; !slices.Equal(got, want) {
		t.Errorf("after a 1 minute deadline: got row keys %q, want %q", got, want)
	}

	if err := drop(3 * time.Minute); err != nil {
		t.Fatalf("with a 3 minute deadline: %v", err)
	}
	if got := rowKeys(t, s, tbl.Name); len(got) != 0 {
		t.Errorf("after a 3 minute deadline: got row keys %q, want none", got)
	}
}

func rowKeys(t *testing.T, s *server, tbl string) []string {
	t.Helper()
	mock := &MockReadRowsServer{}
	if err := s.ReadRows(&btpb.ReadRowsRequest{TableName: tbl}, mock); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range mock.responses {
		for _, chunk := range r.Chunks {
			if len(chunk.RowKey) > 0 {
				keys = append(keys, string(chunk.RowKey))
			}
		}
	}
	return keys
}
