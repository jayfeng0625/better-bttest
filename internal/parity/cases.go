// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"slices"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
)

// Cases is every parity case, in run order. Each aggregate case runs once per aggregate family, named
// "<family>/<name>".
func Cases() []Case {
	var cases []Case
	for _, agg := range Aggregates {
		for _, c := range AggregateCases(agg) {
			c.Name = string(agg) + "/" + c.Name
			cases = append(cases, c)
		}
	}
	return slices.Concat(cases, PlainCases, TableCases())
}

// AggregateCases are data cases on the aggregate family: how each write merges, and which mutations, rules, and
// inputs the family accepts.
func AggregateCases(agg Family) []Case {
	setupAgg := []Call{Mutate(AddToCell(agg, Int(456)))}
	setupPlain := []Call{Mutate(SetCell(Plain, BE(456)))}
	setupBoth := []Call{Mutate(AddToCell(agg, Int(456)), SetCell(Plain, BE(456)))}
	threeBytes := BE(456)[5:]
	nineBytes := append([]byte{0}, BE(456)...)

	return []Case{
		// An application's write and read paths.
		{
			Name: "AddToCell merges at one timestamp",
			Calls: []Call{
				Mutate(AddToCell(agg, Int(456))), Read,
				Mutate(AddToCell(agg, Int(123))), Read,
				Mutate(AddToCell(agg, Int(789))), Read,
			},
		},
		{
			Name: "row write, then a second row write",
			Calls: []Call{
				Mutate(
					SetCell(Plain, BE(1), Col("version")),
					AddToCell(agg, Int(456)),
					SetCell(Plain, BE(123), Col("updatedAt"), At(123000)),
					SetCell(Plain, BE(789), Col("expiresAt"), At(123000)),
					SetCell(Plain, BE(1), Col("flag"), At(789000)),
					SetCell(Plain, []byte(`["default"]`), Col("labels"), At(123000)),
					SetCell(Plain, BE(222), Col("createdAt"), At(222000)),
				), Read,
				Mutate(SetCell(Plain, BE(456), Col("updatedAt"), At(456000)), AddToCell(agg, Int(678))), Read,
			},
		},
		{
			Name: "DeleteFromColumn on the aggregate family, then AddToCell",
			Calls: []Call{
				Mutate(AddToCell(agg, Int(456))), Read,
				Mutate(DeleteFromColumn(agg)), Read,
				Mutate(AddToCell(agg, Int(678))), Read,
			},
		},
		{
			Name: "AddToCell at two timestamps",
			Calls: []Call{
				Mutate(AddToCell(agg, Int(456))), Read,
				Mutate(AddToCell(agg, Int(123), At(2000))), Read,
			},
		},
		{
			Name: "MergeToCell merges at one timestamp",
			Calls: []Call{
				Mutate(MergeToCell(agg, Bytes(BE(456)))), Read,
				Mutate(MergeToCell(agg, Bytes(BE(123)))), Read,
				Mutate(MergeToCell(agg, Bytes(BE(789)))), Read,
			},
		},
		{
			Name: "AddToCell compares negative values as signed",
			Calls: []Call{
				Mutate(AddToCell(agg, Int(-3))), Read,
				Mutate(AddToCell(agg, Int(5))), Read,
			},
		},
		{
			Name: "MergeToCell compares negative values as signed",
			Calls: []Call{
				Mutate(MergeToCell(agg, Bytes(BE(-3)))), Read,
				Mutate(MergeToCell(agg, Bytes(BE(5)))), Read,
			},
		},

		// Which mutations MutateRow accepts on each family type.
		{Name: "MutateRow SetCell on aggregate", Calls: []Call{Mutate(SetCell(agg, BE(456))), Read}},
		{Name: "MutateRow AddToCell on aggregate", Calls: []Call{Mutate(AddToCell(agg, Int(456))), Read}},
		{Name: "MutateRow MergeToCell on aggregate", Calls: []Call{Mutate(MergeToCell(agg, Bytes(BE(456)))), Read}},
		{Name: "MutateRow DeleteFromColumn on aggregate", Setup: setupAgg, Calls: []Call{Mutate(DeleteFromColumn(agg)), Read}},
		{
			Name:  "MutateRow DeleteFromColumn with a time range on aggregate",
			Setup: setupAgg,
			Calls: []Call{Mutate(DeleteFromColumnBetween(agg, 1000, 2000)), Read},
		},
		{Name: "MutateRow DeleteFromFamily on aggregate", Setup: setupBoth, Calls: []Call{Mutate(DeleteFromFamily(agg)), Read}},
		{Name: "MutateRow DeleteFromRow with aggregate cells", Setup: setupBoth, Calls: []Call{Mutate(DeleteFromRow()), Read}},
		{Name: "MutateRow AddToCell then SetCell on aggregate", Calls: []Call{Mutate(AddToCell(agg, Int(456)), SetCell(agg, BE(456))), Read}},
		{Name: "MutateRow SetCell on plain then SetCell on aggregate", Calls: []Call{Mutate(SetCell(Plain, BE(456)), SetCell(agg, BE(456))), Read}},

		// MutateRows batches.
		{
			Name: "MutateRows valid entries",
			Calls: []Call{
				MutateRows{Entries: []Entry{
					{Row: "first", Mutations: Mutations(AddToCell(agg, Int(456)))},
					{Row: "second", Mutations: Mutations(SetCell(Plain, BE(456)))},
				}},
				ReadRow{Row: "first"}, ReadRow{Row: "second"},
			},
		},
		{
			Name: "MutateRows SetCell on aggregate, then a valid entry",
			Calls: []Call{
				MutateRows{Entries: []Entry{
					{Row: "first", Mutations: Mutations(SetCell(agg, BE(456)))},
					{Row: "second", Mutations: Mutations(AddToCell(agg, Int(456)))},
				}},
				ReadRow{Row: "first"}, ReadRow{Row: "second"},
			},
		},
		{
			Name: "MutateRows a valid entry, then SetCell on aggregate",
			Calls: []Call{
				MutateRows{Entries: []Entry{
					{Row: "first", Mutations: Mutations(SetCell(Plain, BE(456)))},
					{Row: "second", Mutations: Mutations(SetCell(agg, BE(456)))},
				}},
				ReadRow{Row: "first"}, ReadRow{Row: "second"},
			},
		},

		// CheckAndMutateRow applies one branch. With no predicate, a row with cells takes the true branch.
		{
			Name:  "CheckAndMutateRow SetCell on aggregate in the applied branch",
			Setup: setupPlain,
			Calls: []Call{CheckAndMutate{True: Mutations(SetCell(agg, BE(456)))}, Read},
		},
		{
			Name:  "CheckAndMutateRow SetCell on aggregate in the branch it skips",
			Setup: setupPlain,
			Calls: []Call{CheckAndMutate{True: Mutations(AddToCell(agg, Int(456))), False: Mutations(SetCell(agg, BE(456)))}, Read},
		},

		// ReadModifyWriteRow.
		{Name: "ReadModifyWriteRow increment on aggregate", Setup: setupAgg, Calls: []Call{ReadModifyWrite{Rules: Rules(Increment(agg))}, Read}},
		{Name: "ReadModifyWriteRow increment on empty aggregate", Calls: []Call{ReadModifyWrite{Rules: Rules(Increment(agg))}, Read}},
		{Name: "ReadModifyWriteRow append on aggregate", Setup: setupAgg, Calls: []Call{ReadModifyWrite{Rules: Rules(Append(agg))}, Read}},
		{Name: "ReadModifyWriteRow append on empty aggregate", Calls: []Call{ReadModifyWrite{Rules: Rules(Append(agg))}, Read}},
		{
			Name:  "ReadModifyWriteRow increment on plain, then on aggregate",
			Setup: setupPlain,
			Calls: []Call{ReadModifyWrite{Rules: Rules(Increment(Plain), Increment(agg))}, Read},
		},

		// Input kinds, checked for the whole request.
		{Name: "MutateRow AddToCell with a bytes input", Calls: []Call{Mutate(AddToCell(agg, Bytes(BE(456)))), Read}},
		{Name: "MutateRow MergeToCell with an int input", Calls: []Call{Mutate(MergeToCell(agg, Int(456))), Read}},
		{Name: "MutateRow MergeToCell with a raw input", Calls: []Call{Mutate(MergeToCell(agg, Raw(BE(456)))), Read}},
		{
			Name:  "MutateRow SetCell on plain, then AddToCell with a bytes input",
			Calls: []Call{Mutate(SetCell(Plain, BE(456)), AddToCell(agg, Bytes(BE(456)))), Read},
		},
		{
			Name: "MutateRows a valid entry, then AddToCell with a bytes input",
			Calls: []Call{
				MutateRows{Entries: []Entry{
					{Row: "first", Mutations: Mutations(SetCell(Plain, BE(456)))},
					{Row: "second", Mutations: Mutations(AddToCell(agg, Bytes(BE(456))))},
				}},
				ReadRow{Row: "first"}, ReadRow{Row: "second"},
			},
		},
		{
			Name:  "CheckAndMutateRow AddToCell with a bytes input in the applied branch",
			Setup: setupPlain,
			Calls: []Call{CheckAndMutate{True: Mutations(SetCell(Plain, BE(456)), AddToCell(agg, Bytes(BE(456))))}, Read},
		},
		{
			Name:  "CheckAndMutateRow AddToCell with a bytes input in the branch it skips",
			Setup: setupPlain,
			Calls: []Call{CheckAndMutate{True: Mutations(SetCell(Plain, BE(456))), False: Mutations(AddToCell(agg, Bytes(BE(456))))}, Read},
		},

		// MergeToCell inputs that are not 8 bytes.
		{Name: "MergeToCell with a 0-byte input on an empty cell", Calls: []Call{Mutate(MergeToCell(agg, Bytes([]byte{}))), Read}},
		{Name: "MergeToCell with a 3-byte input on an empty cell", Calls: []Call{Mutate(MergeToCell(agg, Bytes(threeBytes))), Read}},
		{Name: "MergeToCell with a 9-byte input on an empty cell", Calls: []Call{Mutate(MergeToCell(agg, Bytes(nineBytes))), Read}},
		{
			Name:  "MutateRow SetCell on plain, then MergeToCell with a 3-byte input",
			Calls: []Call{Mutate(SetCell(Plain, BE(456)), MergeToCell(agg, Bytes(threeBytes))), Read},
		},
		{
			Name: "MutateRows MergeToCell with a 3-byte input, then a valid entry",
			Calls: []Call{
				MutateRows{Entries: []Entry{
					{Row: "first", Mutations: Mutations(MergeToCell(agg, Bytes(threeBytes)))},
					{Row: "second", Mutations: Mutations(SetCell(Plain, BE(456)))},
				}},
				ReadRow{Row: "first"}, ReadRow{Row: "second"},
			},
		},
		{
			Name:  "CheckAndMutateRow MergeToCell with a 3-byte input in the applied branch",
			Setup: setupPlain,
			Calls: []Call{CheckAndMutate{True: Mutations(SetCell(Plain, BE(456)), MergeToCell(agg, Bytes(threeBytes)))}, Read},
		},
		{
			Name:  "CheckAndMutateRow MergeToCell with a 3-byte input in the branch it skips",
			Setup: setupPlain,
			Calls: []Call{CheckAndMutate{True: Mutations(SetCell(Plain, BE(456))), False: Mutations(MergeToCell(agg, Bytes(threeBytes)))}, Read},
		},
		{Name: "MergeToCell with a 0-byte input on 456", Setup: setupAgg, Calls: []Call{Mutate(MergeToCell(agg, Bytes([]byte{}))), Read}},
		{Name: "MergeToCell with a 3-byte input on 456", Setup: setupAgg, Calls: []Call{Mutate(MergeToCell(agg, Bytes(threeBytes))), Read}},
		{Name: "MergeToCell with a 9-byte input on 456", Setup: setupAgg, Calls: []Call{Mutate(MergeToCell(agg, Bytes(nineBytes))), Read}},

		// NULL inputs: no input at all, or an input with no kind. They crash Google's stock emulator.
		{Name: "AddToCell with no input on an empty cell", Calls: []Call{Mutate(AddToCell(agg, NoInput)), Read}},
		{Name: "AddToCell with an empty input on an empty cell", Calls: []Call{Mutate(AddToCell(agg, EmptyInput)), Read}},
		{Name: "AddToCell with no input on 456", Setup: setupAgg, Calls: []Call{Mutate(AddToCell(agg, NoInput)), Read}},
		{Name: "MergeToCell with no input on an empty cell", Calls: []Call{Mutate(MergeToCell(agg, NoInput)), Read}},
		{Name: "MergeToCell with an empty input on an empty cell", Calls: []Call{Mutate(MergeToCell(agg, EmptyInput)), Read}},
		{Name: "MergeToCell with no input on 456", Setup: setupAgg, Calls: []Call{Mutate(MergeToCell(agg, NoInput)), Read}},
	}
}

// PlainCases are data cases that name no aggregate family, so they run once.
var PlainCases = []Case{
	{Name: "MutateRow SetCell on plain", Calls: []Call{Mutate(SetCell(Plain, BE(456))), Read}},
	{Name: "MutateRow AddToCell on plain", Calls: []Call{Mutate(AddToCell(Plain, Int(456))), Read}},
	{Name: "MutateRow MergeToCell on plain", Calls: []Call{Mutate(MergeToCell(Plain, Bytes(BE(456)))), Read}},
	{
		Name: "MutateRows AddToCell on plain, then a valid entry",
		Calls: []Call{
			MutateRows{Entries: []Entry{
				{Row: "first", Mutations: Mutations(AddToCell(Plain, Int(456)))},
				{Row: "second", Mutations: Mutations(SetCell(Plain, BE(456)))},
			}},
			ReadRow{Row: "first"}, ReadRow{Row: "second"},
		},
	},
}

var fourFields = Delimited("#", "a", "b", "c", "d")

type labelled struct {
	label  string
	schema *adminpb.Type_Struct
}

// TableCases are cases on the case's table: row key schemas, writes whose keys do not fit one, and calls on a table
// that does not exist.
func TableCases() []Case {
	schemas := []labelled{
		{"four fields", fourFields},
		{"a different delimiter", Delimited("|", "a", "b", "c", "d")},
		{"one field", Delimited("#", "a")},
		{"no fields", Delimited("#")},
		{"no encoding", &adminpb.Type_Struct{}},
	}
	starts := []labelled{
		{"a table with four fields", fourFields},
		{"a table with no schema", nil},
	}

	var cases []Case
	for _, s := range schemas {
		cases = append(cases, Case{
			Name:  "CreateTable with a row key schema with " + s.label,
			Calls: []Call{CreateTable{Schema: s.schema}, GetTable{}},
		})
	}
	// UpdateTable without ignore_warnings, then with it. The update with no schema clears the field.
	for _, s := range append(schemas, labelled{"no schema", nil}) {
		for _, start := range starts {
			cases = append(cases, Case{
				Name:  "UpdateTable row_key_schema with " + s.label + ", on " + start.label,
				Setup: []Call{CreateTable{Schema: start.schema}},
				Calls: []Call{
					SetRowKeySchema{Schema: s.schema}, GetTable{},
					SetRowKeySchema{Schema: s.schema, IgnoreWarnings: true}, GetTable{},
				},
			})
		}
	}

	write := func(key Row) MutateRow {
		return MutateRow{CaseTable: true, Row: key, Mutations: Mutations(SetCell("cf", []byte("v"), Col("q")))}
	}
	return append(cases,
		Case{
			Name:  "UpdateTable row_key_schema on a protected table",
			Setup: []Call{CreateTable{}, SetDeletionProtection{On: true}},
			Calls: []Call{
				SetRowKeySchema{Schema: fourFields}, GetTable{},
				SetRowKeySchema{IgnoreWarnings: true}, GetTable{},
			},
		},
		Case{
			Name:  "Writes with keys that do not fit the row key schema",
			Setup: []Call{CreateTable{Schema: fourFields}},
			Calls: []Call{write("a#b#c#d#e"), write("a"), write("a#\xff\xfe#c#d"), ReadRowKeys{}},
		},
		Case{
			Name: "Calls on a table that does not exist",
			Calls: []Call{
				GetTable{}, DeleteTable{}, SetDeletionProtection{}, ModifyColumnFamilies{}, DropRowRange{},
				GenerateConsistencyToken{}, CheckConsistency{},
				ReadRow{CaseTable: true, Row: "k"},
				write("k"),
				MutateRows{CaseTable: true, Entries: []Entry{{Row: "k", Mutations: Mutations(SetCell("cf", []byte("v"), Col("q")))}}},
				CheckAndMutate{CaseTable: true, Row: "k", True: Mutations(SetCell("cf", []byte("v"), Col("q")))},
				ReadModifyWrite{CaseTable: true, Row: "k", Rules: Rules(Increment("cf"))},
				SampleRowKeys{},
			},
		},
	)
}
