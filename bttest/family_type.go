// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// checkInputKinds returns production's error when an AddToCell input is not an int_value, or a MergeToCell
// input is not a bytes_value. A NULL input passes. prefix is the field path production puts before the
// mutation's index, as in "Error in field 'Mutation list'".
func checkInputKinds(prefix string, muts []*btpb.Mutation) error {
	for i, mut := range muts {
		var want string
		switch mut := mut.Mutation.(type) {
		case *btpb.Mutation_AddToCell_:
			switch mut.AddToCell.GetInput().GetKind().(type) {
			case nil, *btpb.Value_IntValue:
			default:
				want = "int_value"
			}
		case *btpb.Mutation_MergeToCell_:
			switch mut.MergeToCell.GetInput().GetKind().(type) {
			case nil, *btpb.Value_BytesValue:
			default:
				want = "bytes_value"
			}
		}
		if want != "" {
			return status.Errorf(codes.InvalidArgument, "%s : Error in element #%d : Error in field 'input' : must use `%s`", prefix, i, want)
		}
	}
	return nil
}

// fitFamilyTypes reports whether every mutation fits its family's type. SetCell needs a family with no
// aggregate type, and AddToCell and MergeToCell need an aggregate family. An unknown family is left to
// applyMutations.
func fitFamilyTypes(muts []*btpb.Mutation, fs map[string]*columnFamily) bool {
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
			return false
		}
	}
	return true
}

// rulesFitFamilyTypes reports whether every ReadModifyWriteRow rule targets a family with no aggregate type.
func rulesFitFamilyTypes(rules []*btpb.ReadModifyWriteRule, fs map[string]*columnFamily) bool {
	for _, rule := range rules {
		if cf, ok := fs[rule.FamilyName]; ok && cf.valueType.GetAggregateType() != nil {
			return false
		}
	}
	return true
}

// familyTypeMismatch returns production's error for a mutation that does not fit its family's type.
func familyTypeMismatch(tableName string, rowKey []byte) error {
	return status.Errorf(codes.InvalidArgument, "Error while mutating the row '%s' (%s) : Column family type mismatch", rowKey, tableName)
}
