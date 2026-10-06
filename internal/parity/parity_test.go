// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"context"
	"flag"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
)

// A client retries a target that stops answering, so each case gets a deadline.
const caseDeadline = 30 * time.Second

func deadline(c Case) time.Duration {
	if c.Deadline > 0 {
		return c.Deadline
	}
	return caseDeadline
}

var realInstance = flag.String("real", "", "run the cases on the parity table in the real instance <project>/<instance>, and check the gate against it")

// TestParity runs every case on the real table and on the gate, and checks the gate's results against the real
// table's. It runs only with -real.
func TestParity(t *testing.T) {
	if *realInstance == "" {
		t.Skip("run with -real=<project>/<instance>")
	}
	cases := Cases()
	runID := NewRunID(time.Now())
	real := realResults(t, cases, runID)

	ctx, cancel := context.WithTimeout(context.Background(), caseDeadline)
	defer cancel()
	gate, stop, err := StartGate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	for i, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			want, ok := real[c.Name]
			if !ok {
				t.Skip("the case failed on the real table")
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline(c))
			defer cancel()
			got, err := Run(ctx, gate, runID, i+1, c)
			if err != nil {
				t.Fatal(err)
			}
			if d := Diff(want, Normalize(gate.Instance, runID, got)); d != "" {
				t.Error(d)
			}
		})
	}
}

// Each case's normalized results on the real table. The run checks the table's families first, and deletes every case
// row and case table when the test ends.
func realResults(t *testing.T, cases []Case, runID string) map[string][]Result {
	t.Helper()
	parts := strings.Split(*realInstance, "/")
	if len(parts) != 2 {
		t.Fatalf("-real is %q, want <project>/<instance>", *realInstance)
	}
	ctx, cancel := context.WithTimeout(context.Background(), caseDeadline)
	defer cancel()
	real, stop, err := DialReal(ctx, "projects/"+parts[0]+"/instances/"+parts[1])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	tbl, err := real.Admin.GetTable(ctx, &adminpb.GetTableRequest{Name: real.tablePath(parityTable), View: adminpb.Table_SCHEMA_VIEW})
	if err != nil {
		t.Fatalf("read the schema of %s: %v", parityTable, err)
	}
	if problems := SchemaProblems(tbl.ColumnFamilies); len(problems) > 0 {
		t.Fatalf("the parity table's families differ from the cases':\n%s", strings.Join(problems, "\n"))
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := Cleanup(ctx, real, runID); err != nil {
			t.Error(err)
		}
	})

	results := map[string][]Result{}
	for i, c := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), deadline(c))
		got, err := Run(ctx, real, runID, i+1, c)
		cancel()
		if err != nil {
			t.Errorf("on the real table, %s: %v", c.Name, err)
			continue
		}
		results[c.Name] = Normalize(real.Instance, runID, got)
	}
	return results
}
