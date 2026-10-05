// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"encoding/binary"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// A Case is the calls a run makes on each target, in order. Every Setup call must succeed, or the case fails on that
// target. The run compares each of Calls' results across targets.
type Case struct {
	Name  string
	Setup []Call
	Calls []Call
}

// A Call is one RPC. A data call goes to the parity table, where its rows are this case's rows, unless it sets
// CaseTable. The admin calls go to the case's table, which the run deletes when the case ends.
type Call interface{ isCall() }

// A Row names a row. On the parity table the run prefixes it with the run id and the case name. On the case's table it
// is the row key. The zero Row is the case's main row.
type Row string

type MutateRow struct {
	CaseTable bool
	Row       Row
	Mutations []*btpb.Mutation
}

type MutateRows struct{ Entries []Entry }

type Entry struct {
	Row       Row
	Mutations []*btpb.Mutation
}

// CheckAndMutateRow with no predicate, so a row with cells takes the true branch.
type CheckAndMutate struct {
	Row         Row
	True, False []*btpb.Mutation
}

type ReadModifyWrite struct {
	Row   Row
	Rules []*btpb.ReadModifyWriteRule
}

// CreateTable with one family, cf, that keeps one version. A Schema of nil creates the table with no row key schema.
type CreateTable struct{ Schema *adminpb.Type_Struct }

// UpdateTable with the mask row_key_schema. A Schema of nil clears it.
type SetRowKeySchema struct {
	Schema         *adminpb.Type_Struct
	IgnoreWarnings bool
}

// UpdateTable with the mask deletion_protection.
type SetDeletionProtection struct{ On bool }

// ReadRows of one row, keeping each cell's raw bytes.
type ReadRow struct {
	CaseTable bool
	Row       Row
}

// ReadRows over the case's table, keeping only the row keys.
type ReadRowKeys struct{}

// GetTable with SCHEMA_VIEW.
type GetTable struct{}

func (MutateRow) isCall()             {}
func (MutateRows) isCall()            {}
func (CheckAndMutate) isCall()        {}
func (ReadModifyWrite) isCall()       {}
func (CreateTable) isCall()           {}
func (SetRowKeySchema) isCall()       {}
func (SetDeletionProtection) isCall() {}
func (ReadRow) isCall()               {}
func (ReadRowKeys) isCall()           {}
func (GetTable) isCall()              {}

// Builders for mutations at column c and 1000 µs, unless an option says otherwise.

type cellAt struct {
	col string
	ts  int64
}

type Opt func(*cellAt)

func Col(c string) Opt { return func(a *cellAt) { a.col = c } }
func At(ts int64) Opt  { return func(a *cellAt) { a.ts = ts } }

func at(opts []Opt) cellAt {
	a := cellAt{col: "c", ts: 1000}
	for _, o := range opts {
		o(&a)
	}
	return a
}

// BE is n as the 8 big-endian bytes that an int64 aggregate stores.
func BE(n int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(n)) }

func Int(n int64) *btpb.Value    { return &btpb.Value{Kind: &btpb.Value_IntValue{IntValue: n}} }
func Bytes(b []byte) *btpb.Value { return &btpb.Value{Kind: &btpb.Value_BytesValue{BytesValue: b}} }
func Raw(b []byte) *btpb.Value   { return &btpb.Value{Kind: &btpb.Value_RawValue{RawValue: b}} }

// The two NULL inputs: no input at all, and an input with no kind.
var (
	NoInput    *btpb.Value
	EmptyInput = &btpb.Value{}
)

func SetCell(f Family, value []byte, opts ...Opt) *btpb.Mutation {
	a := at(opts)
	return &btpb.Mutation{Mutation: &btpb.Mutation_SetCell_{SetCell: &btpb.Mutation_SetCell{
		FamilyName: string(f), ColumnQualifier: []byte(a.col), TimestampMicros: a.ts, Value: value,
	}}}
}

func AddToCell(f Family, in *btpb.Value, opts ...Opt) *btpb.Mutation {
	a := at(opts)
	return &btpb.Mutation{Mutation: &btpb.Mutation_AddToCell_{AddToCell: &btpb.Mutation_AddToCell{
		FamilyName: string(f), ColumnQualifier: Raw([]byte(a.col)), Timestamp: rawTS(a.ts), Input: in,
	}}}
}

func MergeToCell(f Family, in *btpb.Value, opts ...Opt) *btpb.Mutation {
	a := at(opts)
	return &btpb.Mutation{Mutation: &btpb.Mutation_MergeToCell_{MergeToCell: &btpb.Mutation_MergeToCell{
		FamilyName: string(f), ColumnQualifier: Raw([]byte(a.col)), Timestamp: rawTS(a.ts), Input: in,
	}}}
}

func rawTS(ts int64) *btpb.Value {
	return &btpb.Value{Kind: &btpb.Value_RawTimestampMicros{RawTimestampMicros: ts}}
}

// Mutate is a MutateRow on the case's main row, and Read is a ReadRow of it.
func Mutate(ms ...*btpb.Mutation) MutateRow { return MutateRow{Mutations: ms} }

var Read = ReadRow{}

func Increment(f Family) *btpb.ReadModifyWriteRule {
	return &btpb.ReadModifyWriteRule{FamilyName: string(f), ColumnQualifier: []byte("c"), Rule: &btpb.ReadModifyWriteRule_IncrementAmount{IncrementAmount: 1}}
}

// Delimited is a row key schema of string fields, encoded with the delimiter between them.
func Delimited(delimiter string, fields ...string) *adminpb.Type_Struct {
	s := &adminpb.Type_Struct{Encoding: &adminpb.Type_Struct_Encoding{Encoding: &adminpb.Type_Struct_Encoding_DelimitedBytes_{
		DelimitedBytes: &adminpb.Type_Struct_Encoding_DelimitedBytes{Delimiter: []byte(delimiter)},
	}}}
	for _, f := range fields {
		s.Fields = append(s.Fields, &adminpb.Type_Struct_Field{FieldName: f, Type: &adminpb.Type{Kind: &adminpb.Type_StringType{
			StringType: &adminpb.Type_String{Encoding: &adminpb.Type_String_Encoding{Encoding: &adminpb.Type_String_Encoding_Utf8Bytes_{}}},
		}}})
	}
	return s
}

func DeleteFromColumn(f Family) *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromColumn_{DeleteFromColumn: &btpb.Mutation_DeleteFromColumn{
		FamilyName: string(f), ColumnQualifier: []byte("c"),
	}}}
}

// DeleteFromColumnBetween deletes the cells at or after start and before end.
func DeleteFromColumnBetween(f Family, start, end int64) *btpb.Mutation {
	m := DeleteFromColumn(f)
	m.GetDeleteFromColumn().TimeRange = &btpb.TimestampRange{StartTimestampMicros: start, EndTimestampMicros: end}
	return m
}

func DeleteFromFamily(f Family) *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromFamily_{DeleteFromFamily: &btpb.Mutation_DeleteFromFamily{FamilyName: string(f)}}}
}

func DeleteFromRow() *btpb.Mutation {
	return &btpb.Mutation{Mutation: &btpb.Mutation_DeleteFromRow_{DeleteFromRow: &btpb.Mutation_DeleteFromRow{}}}
}

func Append(f Family) *btpb.ReadModifyWriteRule {
	return &btpb.ReadModifyWriteRule{FamilyName: string(f), ColumnQualifier: []byte("c"), Rule: &btpb.ReadModifyWriteRule_AppendValue{AppendValue: []byte("x")}}
}

func Mutations(ms ...*btpb.Mutation) []*btpb.Mutation { return ms }

func Rules(rs ...*btpb.ReadModifyWriteRule) []*btpb.ReadModifyWriteRule { return rs }
