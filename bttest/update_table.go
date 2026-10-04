// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"slices"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
)

// applyUpdate applies the paths in the update mask of an UpdateTable request.
// The caller holds t.mu.
func (t *table) applyUpdate(req *btapb.UpdateTableRequest) error {
	paths := req.GetUpdateMask().GetPaths()
	if slices.Contains(paths, "row_key_schema") {
		if err := t.updateRowKeySchema(req.GetTable().GetRowKeySchema(), req.GetIgnoreWarnings()); err != nil {
			return err
		}
	}
	if slices.Contains(paths, "deletion_protection") {
		t.isProtected = req.GetTable().GetDeletionProtection()
	}
	return nil
}
