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
// request. The caller holds t.mu.
func (t *table) updateRowKeySchema(req *btapb.UpdateTableRequest) error {
	if !slices.Contains(req.GetUpdateMask().GetPaths(), "row_key_schema") {
		return nil
	}
	// A schema with no fields, such as JSON `rowKeySchema: {}`, clears
	// the row key schema.
	schema := req.GetTable().GetRowKeySchema()
	if len(schema.GetFields()) == 0 {
		schema = nil
	}
	if t.keySchema != nil && schema != nil && !proto.Equal(t.keySchema, schema) {
		return status.Error(codes.InvalidArgument, "Row key schema in-place modification is not allowed.")
	}
	if t.keySchema != nil && schema == nil && !req.GetIgnoreWarnings() {
		return status.Error(codes.InvalidArgument, "Row key schema cannot be cleared without setting ignore_warnings to true.")
	}
	t.keySchema = schema
	return nil
}

func (t *table) rowKeySchema() *btapb.Type_Struct {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.keySchema
}
