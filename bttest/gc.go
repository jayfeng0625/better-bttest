package bttest

import (
	"log"
	"sort"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
)

// applyGC returns the cells that the GC rule keeps. cells are in descending
// timestamp order, and so is the result.
func applyGC(cells []cell, rule *btapb.GcRule) []cell {
	now := time.Now().UnixMicro()
	// A cell's age and rank only grow along the cells, and AND and OR of rules
	// that erase more as both grow do too. So once a cell is erased, every
	// older cell is, and GC keeps a prefix.
	kept := sort.Search(len(cells), func(i int) bool {
		return gcErases(rule, cells[i], i, now)
	})
	expired := 0
	for _, c := range cells[kept:] {
		// At rank 0 no version limit erases c, so age alone does.
		if gcErases(rule, c, 0, now) {
			expired++
		}
	}
	if expired > 0 {
		log.Printf("bttest: GC MaxAge in rule %v deleted %d cells.", rule, expired)
	}
	return cells[:kept]
}

// gcErases reports whether the rule erases cell c, where rank is the number of
// newer cells in the column, and now is in microseconds.
func gcErases(rule *btapb.GcRule, c cell, rank int, now int64) bool {
	switch rule := rule.Rule.(type) {
	case *btapb.GcRule_MaxAge:
		return c.ts < now-rule.MaxAge.AsDuration().Microseconds()
	case *btapb.GcRule_MaxNumVersions:
		return rank >= int(rule.MaxNumVersions)
	case *btapb.GcRule_Intersection_:
		rules := rule.Intersection.GetRules()
		for _, sub := range rules {
			if !gcErases(sub, c, rank, now) {
				return false
			}
		}
		return len(rules) > 0
	case *btapb.GcRule_Union_:
		for _, sub := range rule.Union.GetRules() {
			if gcErases(sub, c, rank, now) {
				return true
			}
		}
	}
	return false
}
