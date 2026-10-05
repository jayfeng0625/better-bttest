// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
)

func TestDiffNamesTheCallThatDiffers(t *testing.T) {
	want := []Result{
		{Call: "MutateRow", Status: Status{Code: codes.OK}},
		{Call: "MutateRow", Status: Status{Code: codes.InvalidArgument, Message: "Column family sum is an aggregate"}},
	}
	got := []Result{
		{Call: "MutateRow", Status: Status{Code: codes.OK}},
		{Call: "MutateRow", Status: Status{Code: codes.OK}},
	}

	diff := Diff(want, got)

	if !strings.Contains(diff, "call 1 MutateRow") || strings.Contains(diff, "call 0") {
		t.Errorf("Diff names the wrong calls:\n%s", diff)
	}
	if !strings.Contains(diff, "Column family sum is an aggregate") {
		t.Errorf("Diff omits the wanted message:\n%s", diff)
	}
}

const testInstance = "projects/acme/instances/prod"

// The real project and instance stay out of the golden files.
func TestNormalizeReplacesTheInstance(t *testing.T) {
	results := []Result{{
		Call:   "MutateRow",
		Status: Status{Code: codes.InvalidArgument, Message: "row 'k' (projects/acme/instances/prod/tables/better-bttest-parity) : bad"},
	}}
	want := []Result{{
		Call:   "MutateRow",
		Status: Status{Code: codes.InvalidArgument, Message: "row 'k' (projects/<project>/instances/<instance>/tables/better-bttest-parity) : bad"},
	}}

	if d := cmp.Diff(want, Normalize(testInstance, testRunID, results)); d != "" {
		t.Errorf("Normalize (-want +got):\n%s", d)
	}
}

// Production's DeleteTable message names the project by its number, in braces.
func TestNormalizeReplacesTheInstanceWithAProjectNumber(t *testing.T) {
	results := []Result{{
		Call:   "DeleteTable",
		Status: Status{Code: codes.NotFound, Message: "Failed to read: projects/{123456789012}/instances/prod/tables/t"},
	}}
	want := []Result{{
		Call:   "DeleteTable",
		Status: Status{Code: codes.NotFound, Message: "Failed to read: projects/{<project>}/instances/<instance>/tables/t"},
	}}

	if d := cmp.Diff(want, Normalize(testInstance, testRunID, results)); d != "" {
		t.Errorf("Normalize (-want +got):\n%s", d)
	}
}

func TestNormalizeReplacesTheRunID(t *testing.T) {
	results := []Result{{
		Call:    "MutateRows",
		Status:  Status{Code: codes.InvalidArgument, Message: "table better-bttest-parity-0123456789ab-t3"},
		Entries: []Status{{Code: codes.InvalidArgument, Message: "row probe#0123456789ab#sum/x"}},
	}}
	want := []Result{{
		Call:    "MutateRows",
		Status:  Status{Code: codes.InvalidArgument, Message: "table better-bttest-parity-<run>-t3"},
		Entries: []Status{{Code: codes.InvalidArgument, Message: "row probe#<run>#sum/x"}},
	}}

	if d := cmp.Diff(want, Normalize(testInstance, testRunID, results)); d != "" {
		t.Errorf("Normalize (-want +got):\n%s", d)
	}
}

func TestNormalizeMarksServerClockTimes(t *testing.T) {
	results := []Result{{Call: "ReadRow", Cells: []Cell{
		{Column: "sum:c", TS: 1000, Value: []byte{1}},
		{Column: "sum:d", TS: 1_759_000_000_000_000, Value: []byte{2}},
	}}}
	want := []Result{{Call: "ReadRow", Cells: []Cell{
		{Column: "sum:c", TS: 1000, Value: []byte{1}},
		{Column: "sum:d", TS: -1, Value: []byte{2}},
	}}}

	if d := cmp.Diff(want, Normalize(testInstance, testRunID, results)); d != "" {
		t.Errorf("Normalize (-want +got):\n%s", d)
	}
}

// The ReadRows API specifies the order within a family, by qualifier and then by decreasing time, and leaves the order
// of families unspecified.
func TestNormalizeOrdersFamiliesAndKeepsTheOrderWithinEach(t *testing.T) {
	results := []Result{{Call: "ReadRow", Cells: []Cell{
		{Column: "sum:c", TS: 1000},
		{Column: "plain:b", TS: 2000},
		{Column: "plain:b", TS: 1000},
		{Column: "plain:a", TS: 1000},
	}}}
	want := []Result{{Call: "ReadRow", Cells: []Cell{
		{Column: "plain:b", TS: 2000},
		{Column: "plain:b", TS: 1000},
		{Column: "plain:a", TS: 1000},
		{Column: "sum:c", TS: 1000},
	}}}

	if d := cmp.Diff(want, Normalize(testInstance, testRunID, results)); d != "" {
		t.Errorf("Normalize (-want +got):\n%s", d)
	}
}

// Production's messages, as two calls returned them.
func TestNormalizeTrimsTheStructEncodingMessage(t *testing.T) {
	results := []Result{
		{Call: "CreateTable", Status: Status{Code: codes.InvalidArgument, Message: "Missing encoding for STRUCT: go/debugproto  \n"}},
		{Call: "SetRowKeySchema", Status: Status{Code: codes.InvalidArgument, Message: "Missing encoding for STRUCT: go/debugstr    \n"}},
	}
	want := []Result{
		{Call: "CreateTable", Status: Status{Code: codes.InvalidArgument, Message: "Missing encoding for STRUCT"}},
		{Call: "SetRowKeySchema", Status: Status{Code: codes.InvalidArgument, Message: "Missing encoding for STRUCT"}},
	}

	if d := cmp.Diff(want, Normalize(testInstance, testRunID, results)); d != "" {
		t.Errorf("Normalize (-want +got):\n%s", d)
	}
}

func TestDiffReportsADifferentNumberOfCalls(t *testing.T) {
	want := []Result{{Call: "MutateRow"}, {Call: "ReadRow"}}
	got := []Result{{Call: "MutateRow"}}

	diff := Diff(want, got)

	if !strings.Contains(diff, "want 2 calls, got 1") {
		t.Errorf("Diff omits the call counts:\n%s", diff)
	}
}
