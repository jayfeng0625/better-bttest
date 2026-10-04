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
// request, by production's rules:
//   - A schema with no encoding is invalid.
//   - A table with no row key schema takes any valid schema.
//   - A table with a row key schema rejects a different schema, even with
//     ignore_warnings.
//   - A request that leaves the schema unset clears the row key schema,
//     and needs ignore_warnings to do so.
//
// The caller holds t.mu.
func (t *table) updateRowKeySchema(req *btapb.UpdateTableRequest) error {
	if !slices.Contains(req.GetUpdateMask().GetPaths(), "row_key_schema") {
		return nil
	}
	schema := req.GetTable().GetRowKeySchema()
	if err := validRowKeySchema(schema); err != nil {
		return err
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

// validRowKeySchema checks a row key schema sent on CreateTable or
// UpdateTable. A nil schema is valid.
func validRowKeySchema(schema *btapb.Type_Struct) error {
	if schema != nil && schema.GetEncoding() == nil {
		return status.Error(codes.InvalidArgument, "Missing encoding for STRUCT")
	}
	return nil
}

func (t *table) rowKeySchema() *btapb.Type_Struct {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.keySchema
}
