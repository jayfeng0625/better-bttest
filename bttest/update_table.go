// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"slices"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
)

// applyUpdate applies the paths in the update mask of an UpdateTable request.
// The caller holds t.mu.
func (t *table) applyUpdate(req *btapb.UpdateTableRequest) error {
	if err := t.updateRowKeySchema(req); err != nil {
		return err
	}
	if slices.Contains(req.GetUpdateMask().GetPaths(), "deletion_protection") {
		t.isProtected = req.GetTable().GetDeletionProtection()
	}
	return nil
}
