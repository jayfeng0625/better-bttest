// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
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
