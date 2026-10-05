// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"context"
	"testing"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Production's errors for an RPC that names a table that does not exist, as a parity run recorded them.
func TestCallsOnMissingTableReturnNotFound(t *testing.T) {
	const name = "projects/p/instances/i/tables/missing"
	const notFound = "Not found: " + name
	setCell := []*btpb.Mutation{setCellIn("cf", 456)}
	tests := []struct {
		name string
		call func(context.Context, *server) error
		want string
	}{
		{"GetTable", func(ctx context.Context, s *server) error {
			_, err := s.GetTable(ctx, &btapb.GetTableRequest{Name: name, View: btapb.Table_SCHEMA_VIEW})
			return err
		}, notFound},
		{"DeleteTable", func(ctx context.Context, s *server) error {
			_, err := s.DeleteTable(ctx, &btapb.DeleteTableRequest{Name: name})
			return err
		}, "Failed to read: projects/{p}/instances/i/tables/missing"},
		{"UpdateTable", func(ctx context.Context, s *server) error {
			_, err := s.UpdateTable(ctx, &btapb.UpdateTableRequest{
				Table:      &btapb.Table{Name: name, DeletionProtection: true},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"deletion_protection"}},
			})
			return err
		}, notFound},
		{"ModifyColumnFamilies", func(ctx context.Context, s *server) error {
			_, err := s.ModifyColumnFamilies(ctx, &btapb.ModifyColumnFamiliesRequest{
				Name: name,
				Modifications: []*btapb.ModifyColumnFamiliesRequest_Modification{{
					Id: "cf2", Mod: &btapb.ModifyColumnFamiliesRequest_Modification_Create{Create: &btapb.ColumnFamily{}},
				}},
			})
			return err
		}, notFound},
		{"DropRowRange", func(ctx context.Context, s *server) error {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			_, err := s.DropRowRange(ctx, &btapb.DropRowRangeRequest{
				Name: name, Target: &btapb.DropRowRangeRequest_DeleteAllDataFromTable{DeleteAllDataFromTable: true},
			})
			return err
		}, notFound},
		{"GenerateConsistencyToken", func(ctx context.Context, s *server) error {
			_, err := s.GenerateConsistencyToken(ctx, &btapb.GenerateConsistencyTokenRequest{Name: name})
			return err
		}, notFound},
		{"CheckConsistency", func(ctx context.Context, s *server) error {
			_, err := s.CheckConsistency(ctx, &btapb.CheckConsistencyRequest{Name: name, ConsistencyToken: "token"})
			return err
		}, notFound},
		{"ReadRows", func(ctx context.Context, s *server) error {
			return s.ReadRows(&btpb.ReadRowsRequest{TableName: name}, &MockReadRowsServer{})
		}, notFound},
		{"MutateRow", func(ctx context.Context, s *server) error {
			_, err := s.MutateRow(ctx, &btpb.MutateRowRequest{TableName: name, RowKey: []byte("k"), Mutations: setCell})
			return err
		}, notFound},
		{"MutateRows", func(ctx context.Context, s *server) error {
			return s.MutateRows(&btpb.MutateRowsRequest{
				TableName: name, Entries: []*btpb.MutateRowsRequest_Entry{{RowKey: []byte("k"), Mutations: setCell}},
			}, &mutateRowsRecorder{})
		}, notFound},
		{"CheckAndMutateRow", func(ctx context.Context, s *server) error {
			_, err := s.CheckAndMutateRow(ctx, &btpb.CheckAndMutateRowRequest{TableName: name, RowKey: []byte("k"), TrueMutations: setCell})
			return err
		}, notFound},
		{"ReadModifyWriteRow", func(ctx context.Context, s *server) error {
			_, err := s.ReadModifyWriteRow(ctx, &btpb.ReadModifyWriteRowRequest{
				TableName: name, RowKey: []byte("k"), Rules: []*btpb.ReadModifyWriteRule{{
					FamilyName: "cf", ColumnQualifier: []byte("q"), Rule: &btpb.ReadModifyWriteRule_IncrementAmount{IncrementAmount: 1},
				}},
			})
			return err
		}, notFound},
		{"SampleRowKeys", func(ctx context.Context, s *server) error {
			return s.SampleRowKeys(&btpb.SampleRowKeysRequest{TableName: name}, &MockSampleRowKeysServer{})
		}, notFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &server{tables: make(map[string]*table)}
			wantStatus(t, tc.call(context.Background(), s), codes.NotFound, tc.want)
		})
	}
}
