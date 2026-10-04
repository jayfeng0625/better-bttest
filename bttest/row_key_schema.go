// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// updateRowKeySchema sets the row key schema from an UpdateTable request, by
// production's rules:
//   - A schema with no encoding is invalid.
//   - A table with no row key schema takes any valid schema.
//   - A table with a row key schema rejects a different schema, even with
//     ignore_warnings.
//   - A table with a row key schema clears it on a request that leaves the
//     schema unset, and only with ignore_warnings.
//
// The caller holds t.mu.
func (t *table) updateRowKeySchema(schema *btapb.Type_Struct, ignoreWarnings bool) error {
	if err := validateRowKeySchema(schema); err != nil {
		return err
	}
	if t.rowKeySchema != nil && schema != nil && !proto.Equal(t.rowKeySchema, schema) {
		return status.Error(codes.InvalidArgument, "Row key schema in-place modification is not allowed.")
	}
	if t.rowKeySchema != nil && schema == nil && !ignoreWarnings {
		return status.Error(codes.InvalidArgument, "Row key schema cannot be cleared without setting ignore_warnings to true.")
	}
	t.rowKeySchema = schema
	return nil
}

func validateRowKeySchema(schema *btapb.Type_Struct) error {
	if schema != nil && schema.GetEncoding() == nil {
		return status.Error(codes.InvalidArgument, "Missing encoding for STRUCT")
	}
	return nil
}

func (t *table) getRowKeySchema() *btapb.Type_Struct {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.rowKeySchema
}
