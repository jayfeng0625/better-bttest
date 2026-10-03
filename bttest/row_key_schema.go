// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"slices"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// updateRowKeySchema applies the row_key_schema path of an UpdateTable
// request under production's rules. The caller holds t.mu.
func (t *table) updateRowKeySchema(req *btapb.UpdateTableRequest) error {
	if !slices.Contains(req.GetUpdateMask().GetPaths(), "row_key_schema") {
		return nil
	}
	// A schema with no fields, such as JSON `rowKeySchema: {}`, clears it.
	schema := req.GetTable().GetRowKeySchema()
	if len(schema.GetFields()) == 0 {
		schema = nil
	}
	if t.rowKeySchema != nil && schema != nil && !proto.Equal(t.rowKeySchema, schema) {
		return status.Error(codes.InvalidArgument, "Row key schema in-place modification is not allowed.")
	}
	if t.rowKeySchema != nil && schema == nil && !req.GetIgnoreWarnings() {
		return status.Error(codes.InvalidArgument, "Row key schema cannot be cleared without setting ignore_warnings to true.")
	}
	t.rowKeySchema = schema
	return nil
}

// getRowKeySchema returns the table's row key schema under t.mu.
func (t *table) getRowKeySchema() *btapb.Type_Struct {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.rowKeySchema
}
