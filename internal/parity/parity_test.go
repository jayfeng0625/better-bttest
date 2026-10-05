// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
)

// The real table's normalized results, by case name, which the gate must match.
const goldenPath = "testdata/real.json"

// A client retries a target that stops answering, so each case gets a deadline.
const caseDeadline = 30 * time.Second

var (
	realInstance = flag.String("real", "", "run the cases on the parity table in the real instance <project>/<instance> too, and check the gate against it")
	update       = flag.Bool("update", false, "with -real, write the real table's results to "+goldenPath)
)

// TestParity runs every case on the gate, and checks its results against the real table's. With -real, the run
// gets the real table's results from the table, and otherwise from the golden file.
func TestParity(t *testing.T) {
	cases := Cases()
	runID := newRunID(t)
	var golden map[string][]Result
	switch {
	case *realInstance != "":
		golden = realResults(t, cases, runID)
		if *update {
			if t.Failed() {
				t.Fatalf("a case failed on the real table, so %s stays as it is", goldenPath)
			}
			writeGolden(t, golden)
		}
	case *update:
		t.Fatal("-update needs -real")
	default:
		golden = readGolden(t)
	}

	ctx, cancel := context.WithTimeout(context.Background(), caseDeadline)
	defer cancel()
	gate, stop, err := StartGate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	names := map[string]bool{}
	for i, c := range cases {
		names[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			want, ok := golden[c.Name]
			if !ok {
				t.Fatalf("%s has no result for the case. Run the cases on the real table with -update.", goldenPath)
			}
			ctx, cancel := context.WithTimeout(context.Background(), caseDeadline)
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
	for name := range golden {
		if !names[name] {
			t.Errorf("%s has a result for %q, which no case has. Run the cases on the real table with -update.", goldenPath, name)
		}
	}
}

func newRunID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func readGolden(t *testing.T) map[string][]Result {
	t.Helper()
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("%v. Run the cases on the real table with -update.", err)
	}
	var golden map[string][]Result
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
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
		if err := Cleanup(ctx, real); err != nil {
			t.Error(err)
		}
	})

	results := map[string][]Result{}
	for i, c := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), caseDeadline)
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

// Write the golden file. Normalize replaces the real project and instance, and the write fails if either remains.
func writeGolden(t *testing.T, golden map[string][]Result) {
	t.Helper()
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(golden); err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Split(*realInstance, "/") {
		if bytes.Contains(data.Bytes(), []byte(name)) {
			t.Fatalf("the results name %q, which must stay out of %s", name, goldenPath)
		}
	}
	if err := os.WriteFile(goldenPath, data.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
