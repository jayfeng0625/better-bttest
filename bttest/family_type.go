// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// checkFamilyTypes returns production's error when a mutation does not fit its family's type.
// SetCell needs a family with no aggregate type, and AddToCell and MergeToCell need an aggregate family.
// Deletes fit either. An unknown family is left to applyMutations.
func checkFamilyTypes(tableName string, rowKey []byte, muts []*btpb.Mutation, fs map[string]*columnFamily) error {
	for _, mut := range muts {
		var family string
		var aggregate bool
		switch mut := mut.Mutation.(type) {
		case *btpb.Mutation_SetCell_:
			family = mut.SetCell.FamilyName
		case *btpb.Mutation_AddToCell_:
			family, aggregate = mut.AddToCell.FamilyName, true
		case *btpb.Mutation_MergeToCell_:
			family, aggregate = mut.MergeToCell.FamilyName, true
		default:
			continue
		}
		if cf, ok := fs[family]; ok && (cf.valueType.GetAggregateType() != nil) != aggregate {
			return familyTypeMismatch(tableName, rowKey)
		}
	}
	return nil
}

func familyTypeMismatch(tableName string, rowKey []byte) error {
	return status.Errorf(codes.InvalidArgument, "Error while mutating the row '%s' (%s) : Column family type mismatch", rowKey, tableName)
}
