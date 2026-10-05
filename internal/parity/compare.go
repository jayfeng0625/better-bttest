// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"fmt"
	"slices"
	"strings"

	"cloud.google.com/go/bigtable"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
)

// A time above this is the server's clock, as in a ReadModifyWriteRow cell. The cases write times far below it.
const serverClockMicros = 1e15

// The instance as the golden files name it, so the real project and instance stay out of the repo.
const goldenInstance = "projects/<project>/instances/<instance>"

// Normalize returns results as the golden files keep them, with what differs between targets and runs replaced. Status
// messages embed table paths, which carry the target's instance, and row keys and table names, which carry the run id.
// A cell at the server's clock gets ServerTime, the time that SetCell takes for the server's clock. ReadRows leaves the
// order of a row's families unspecified, so the cells sort by family, keeping their order within each.
func Normalize(instance, runID string, results []Result) []Result {
	out := make([]Result, len(results))
	for i, r := range results {
		r.Status = normalizeStatus(instance, runID, r.Status)
		r.Entries = slices.Clone(r.Entries)
		for j, e := range r.Entries {
			r.Entries[j] = normalizeStatus(instance, runID, e)
		}
		r.Cells = slices.Clone(r.Cells)
		slices.SortStableFunc(r.Cells, func(a, b Cell) int { return strings.Compare(family(a), family(b)) })
		for j, c := range r.Cells {
			if c.TS > serverClockMicros {
				r.Cells[j].TS = int64(bigtable.ServerTime)
			}
		}
		out[i] = r
	}
	return out
}

func family(c Cell) string {
	f, _, _ := strings.Cut(c.Column, ":")
	return f
}

// Production's message for a row key schema with no encoding continues with text that differs between calls.
const structMessage = "Missing encoding for STRUCT"

func normalizeStatus(instance, runID string, s Status) Status {
	s.Message = strings.ReplaceAll(strings.ReplaceAll(s.Message, instance, goldenInstance), runID, "<run>")
	if strings.HasPrefix(s.Message, structMessage) {
		s.Message = structMessage
	}
	return s
}

// Diff returns each call whose result differs between want and got, or "" when every result matches.
func Diff(want, got []Result) string {
	var b strings.Builder
	if len(want) != len(got) {
		fmt.Fprintf(&b, "want %d calls, got %d\n", len(want), len(got))
	}
	for i := range min(len(want), len(got)) {
		if d := cmp.Diff(want[i], got[i], protocmp.Transform()); d != "" {
			fmt.Fprintf(&b, "call %d %s (-want +got):\n%s", i, want[i].Call, d)
		}
	}
	return b.String()
}
