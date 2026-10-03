// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"log"
	"sort"
	"sync"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
)

var gcTypeWarn sync.Once

// applyGC applies the given GC rule to the cells.
// cells are in descending timestamp order, and so is the result.
func applyGC(cells []cell, rule *btapb.GcRule) []cell {
	now := time.Now().UnixMicro()
	// A cell's age and rank only grow along the cells. MaxAge and version rules
	// erase more cells as both grow, and so do intersection and union rules
	// made of them. So once a cell is erased, every older cell is, and GC keeps
	// a prefix.
	kept := sort.Search(len(cells), func(i int) bool {
		erases, _ := gcErases(rule, cells[i], i, now)
		return erases
	})
	if kept == len(cells) {
		return cells
	}
	deleted := make(map[*btapb.GcRule]int)
	for i, c := range cells[kept:] {
		_, maxAge := gcErases(rule, c, kept+i, now)
		deleted[maxAge]++
	}
	for _, maxAge := range maxAgeRules(rule) {
		if n := deleted[maxAge]; n > 0 {
			log.Printf("bttest: GC MaxAge(%v) deleted %d cells.", maxAge.GetMaxAge(), n)
		}
	}
	return cells[:kept]
}

// gcErases reports whether the rule erases cell c, where rank is the number of
// newer cells in the column, and now is in microseconds. As in production, an
// intersection rule erases a cell only when every one of its rules would, and
// a union rule erases a cell when any of them would. See
// https://cloud.google.com/bigtable/docs/garbage-collection#combinations.
// When the rule erases c, gcErases also returns the MaxAge rule that the GC
// log credits with the deletion, or nil when it credits none. A union passes
// on the credit of its first rule that erases c. An intersection passes on the
// first credit among its rules.
func gcErases(rule *btapb.GcRule, c cell, rank int, now int64) (bool, *btapb.GcRule) {
	switch r := rule.Rule.(type) {
	default:
		gcTypeWarn.Do(func() {
			log.Printf("Unsupported GC rule type %T", r)
		})
	case *btapb.GcRule_MaxAge:
		return c.ts < now-r.MaxAge.AsDuration().Microseconds(), rule
	case *btapb.GcRule_MaxNumVersions:
		return rank >= int(r.MaxNumVersions), nil
	case *btapb.GcRule_Intersection_:
		rules := r.Intersection.GetRules()
		var credit *btapb.GcRule
		for _, sub := range rules {
			erases, maxAge := gcErases(sub, c, rank, now)
			if !erases {
				return false, nil
			}
			if credit == nil {
				credit = maxAge
			}
		}
		return len(rules) > 0, credit
	case *btapb.GcRule_Union_:
		// Upstream applies a union's rules in turn, so the first rule that
		// erases c is the one that deletes it.
		for _, sub := range r.Union.GetRules() {
			if erases, maxAge := gcErases(sub, c, rank, now); erases {
				return true, maxAge
			}
		}
	}
	return false, nil
}

// maxAgeRules returns the MaxAge rules in rule, in the order upstream applies
// a union's rules.
func maxAgeRules(rule *btapb.GcRule) []*btapb.GcRule {
	var subs []*btapb.GcRule
	switch r := rule.Rule.(type) {
	case *btapb.GcRule_MaxAge:
		return []*btapb.GcRule{rule}
	case *btapb.GcRule_Union_:
		subs = r.Union.GetRules()
	case *btapb.GcRule_Intersection_:
		subs = r.Intersection.GetRules()
	}
	var rules []*btapb.GcRule
	for _, sub := range subs {
		rules = append(rules, maxAgeRules(sub)...)
	}
	return rules
}
