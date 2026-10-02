// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"context"
	"testing"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestGCRules(t *testing.T) {
	tests := []struct {
		name           string
		rule           *btapb.GcRule
		ages, wantAges []time.Duration
	}{
		{
			name:     "intersection over old cells keeps the newest N",
			rule:     gcIntersection(gcMaxAge(time.Hour), gcMaxNumVersions(2)),
			ages:     []time.Duration{2 * time.Hour, 3 * time.Hour, 4 * time.Hour},
			wantAges: []time.Duration{2 * time.Hour, 3 * time.Hour},
		},
		{
			name:     "intersection keeps a lone expired cell",
			rule:     gcIntersection(gcMaxAge(time.Hour), gcMaxNumVersions(1)),
			ages:     []time.Duration{2 * time.Hour},
			wantAges: []time.Duration{2 * time.Hour},
		},
		{
			// The 2h and 3h cells are beyond the first part's 1 version but
			// younger than its 7 days. The second part alone erases the 4h cell.
			name: "union erases a cell that any intersection erases",
			rule: gcUnion(
				gcIntersection(gcMaxNumVersions(1), gcMaxAge(7*24*time.Hour)),
				gcIntersection(gcMaxNumVersions(3), gcMaxAge(time.Hour)),
			),
			ages:     []time.Duration{10 * time.Minute, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour, 8 * 24 * time.Hour},
			wantAges: []time.Duration{10 * time.Minute, 2 * time.Hour, 3 * time.Hour},
		},
		{
			// The first part erases the 30m cell, which is the second newest,
			// but the second part keeps it.
			name: "intersection keeps a cell that any union keeps",
			rule: gcIntersection(
				gcUnion(gcMaxNumVersions(1), gcMaxAge(time.Hour)),
				gcUnion(gcMaxNumVersions(2), gcMaxAge(24*time.Hour)),
			),
			ages:     []time.Duration{10 * time.Minute, 30 * time.Minute, 2 * time.Hour, 48 * time.Hour},
			wantAges: []time.Duration{10 * time.Minute, 30 * time.Minute},
		},
		{
			name:     "empty intersection keeps every cell",
			rule:     gcIntersection(),
			ages:     []time.Duration{2 * time.Hour, 3 * time.Hour},
			wantAges: []time.Duration{2 * time.Hour, 3 * time.Hour},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := &server{tables: make(map[string]*table)}
			now := time.Now()
			tblName := createGCTable(t, s, tc.rule)
			for _, age := range tc.ages {
				if _, err := s.MutateRow(ctx, setCell(tblName, cellTimestamp(now, age))); err != nil {
					t.Fatalf("MutateRow: %v", err)
				}
			}

			s.tables[tblName].gc()

			var want []int64
			for _, age := range tc.wantAges {
				want = append(want, cellTimestamp(now, age))
			}
			if diff := cmp.Diff(want, readCellTimestamps(t, s, tblName)); diff != "" {
				t.Errorf("cell timestamps after GC (-want +got):\n%s", diff)
			}
		})
	}
}

func gcMaxAge(d time.Duration) *btapb.GcRule {
	return &btapb.GcRule{Rule: &btapb.GcRule_MaxAge{MaxAge: durationpb.New(d)}}
}

func gcMaxNumVersions(n int32) *btapb.GcRule {
	return &btapb.GcRule{Rule: &btapb.GcRule_MaxNumVersions{MaxNumVersions: n}}
}

func gcIntersection(rules ...*btapb.GcRule) *btapb.GcRule {
	return &btapb.GcRule{Rule: &btapb.GcRule_Intersection_{Intersection: &btapb.GcRule_Intersection{Rules: rules}}}
}

func gcUnion(rules ...*btapb.GcRule) *btapb.GcRule {
	return &btapb.GcRule{Rule: &btapb.GcRule_Union_{Union: &btapb.GcRule_Union{Rules: rules}}}
}

func createGCTable(t *testing.T, s *server, rule *btapb.GcRule) string {
	t.Helper()
	tbl, err := s.CreateTable(context.Background(), &btapb.CreateTableRequest{
		Parent:  "cluster",
		TableId: "t",
		Table: &btapb.Table{
			ColumnFamilies: map[string]*btapb.ColumnFamily{"cf": {GcRule: rule}},
		},
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	return tbl.Name
}

// cellTimestamp returns the timestamp of a cell that is age old at now,
// truncated to milliseconds because tables default to MILLIS granularity.
func cellTimestamp(now time.Time, age time.Duration) int64 {
	return now.Add(-age).UnixMilli() * microsPerMilli
}
