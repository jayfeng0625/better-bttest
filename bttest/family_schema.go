// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"slices"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// storedGCRule returns rule as production stores it: a rule with nothing in it is no rule.
func storedGCRule(rule *btapb.GcRule) *btapb.GcRule {
	if rule.GetRule() == nil {
		return nil
	}
	return rule
}

// storedValueType returns t as production stores it: a Sum, MIN or MAX aggregate gets its output-only state type. The
// proto makes the state type a function of the input type and the aggregator, and for these three production returns
// the input type.
func storedValueType(t *btapb.Type) *btapb.Type {
	if !int64Aggregate(t) {
		return t
	}
	t = proto.CloneOf(t)
	agg := t.GetAggregateType()
	agg.StateType = proto.CloneOf(agg.InputType)
	return t
}

// checkValueTypeUpdates returns production's error when an update with value_type in its mask would change a family's
// value type. Production refuses the whole request, so the check runs before any modification applies. An update to
// the family's own type passes.
func checkValueTypeUpdates(mods []*btapb.ModifyColumnFamiliesRequest_Modification, fs map[string]*columnFamily) error {
	for _, mod := range mods {
		cf, ok := fs[mod.Id]
		if !ok || mod.GetUpdate() == nil || !slices.Contains(mod.GetUpdateMask().GetPaths(), "value_type") {
			continue
		}
		if !proto.Equal(storedValueType(mod.GetUpdate().GetValueType()), cf.valueType) {
			return status.Error(codes.InvalidArgument, "Immutable fields 'value_type.aggregate_type,sql_type' cannot be updated.")
		}
	}
	return nil
}
