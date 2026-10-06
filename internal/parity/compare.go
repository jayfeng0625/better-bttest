// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"cloud.google.com/go/bigtable"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
)

// A time above this is the server's clock, as in a ReadModifyWriteRow cell. The cases write times far below it.
const serverClockMicros = 1e15

// The instance as Normalize names it, the same on every target. Production's DeleteTable message names the project by
// its number, in braces.
const (
	placeholderInstance         = "projects/<project>/instances/<instance>"
	placeholderNumberedInstance = "projects/{<project>}/instances/<instance>"
)

var numberedInstance = regexp.MustCompile(`projects/\{[^}]*\}/instances/[^/]+`)

// Normalize returns results with what differs between targets and runs replaced. Status
// messages embed table paths, which carry the target's instance, and row keys and table names, which carry the run id.
// A cell at the server's clock gets ServerTime, the time that SetCell takes for the server's clock. ReadRows leaves the
// order of a row's families unspecified, so the cells sort by family, keeping their order within each.
func Normalize(instance, runID string, results []Result) []Result {
	replacer := strings.NewReplacer(instance, placeholderInstance, runID, "<run>")
	status := func(s Status) Status {
		s.Message = numberedInstance.ReplaceAllLiteralString(replacer.Replace(s.Message), placeholderNumberedInstance)
		if strings.HasPrefix(s.Message, structMessage) {
			s.Message = structMessage
		}
		s.Message, _, _ = strings.Cut(s.Message, evaluatingLine)
		return s
	}
	out := make([]Result, len(results))
	for i, r := range results {
		r.Status = status(r.Status)
		r.Entries = slices.Clone(r.Entries)
		for j, e := range r.Entries {
			r.Entries[j] = status(e)
		}
		r.Cells = cells(r.Cells)
		r.ViewRows = slices.Clone(r.ViewRows)
		for j, vr := range r.ViewRows {
			r.ViewRows[j].Cells = cells(vr.Cells)
		}
		out[i] = r
	}
	return out
}

// The cells sorted by family, with server clock times marked.
func cells(in []Cell) []Cell {
	out := slices.Clone(in)
	slices.SortStableFunc(out, func(a, b Cell) int { return strings.Compare(family(a), family(b)) })
	for j, c := range out {
		if c.TS > serverClockMicros {
			out[j].TS = int64(bigtable.ServerTime)
		}
	}
	return out
}

func family(c Cell) string {
	f, _, _ := strings.Cut(c.Column, ":")
	return f
}

// Production's message for a row key schema with no encoding continues with text that differs between calls.
const structMessage = "Missing encoding for STRUCT"

// Production's runtime SQL error continues on a second line with the expression it was evaluating, in production's
// internal names, such as to_int64_big_endian(`$col3`). The emulator does not reproduce that line, so the comparison
// stops at it.
const evaluatingLine = "\n(while evaluating "

// Diff returns each call whose result differs between want and got, or "" when every result matches. It compares
// bytes with bytes.Equal, because cmp compares a slice element by element, which took a minute on a 7.5 MiB result.
func Diff(want, got []Result) string {
	var b strings.Builder
	if len(want) != len(got) {
		fmt.Fprintf(&b, "want %d calls, got %d\n", len(want), len(got))
	}
	for i := range min(len(want), len(got)) {
		if d := cmp.Diff(want[i], got[i], protocmp.Transform(), cmp.Comparer(bytes.Equal)); d != "" {
			fmt.Fprintf(&b, "call %d %s (-want +got):\n%s", i, want[i].Call, d)
		}
	}
	return b.String()
}
