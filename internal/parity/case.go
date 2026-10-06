// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"context"
	"encoding/binary"
	"time"

	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
)

// A Case is the calls a run makes on each target, in order. Every Setup call must succeed, or the case fails on that
// target. The run compares each of Calls' results across targets.
type Case struct {
	Name  string
	Setup []Call
	Calls []Call
	// Deadline bounds the case's calls when it is set. The tests use caseDeadline otherwise.
	Deadline time.Duration
}

// A Call is one RPC. A data call goes to the parity table, where its rows are this case's rows, unless it sets
// CaseTable, which sends it to the case's table, where its Row is the row key. The other calls go to the case's table,
// which the run deletes when the case ends.
type Call interface {
	run(ctx context.Context, r *runner) (Result, error)
}

// A Row names a row. On the parity table the run prefixes it with the run id and the case name. On the case's table it
// is the row key. The zero Row is the case's main row.
type Row string

type MutateRow struct {
	CaseTable bool
	Row       Row
	Mutations []*btpb.Mutation
}

type MutateRows struct {
	CaseTable bool
	Entries   []Entry
}

type Entry struct {
	Row       Row
	Mutations []*btpb.Mutation
}

// CheckAndMutateRow with no predicate, so a row with cells takes the true branch.
type CheckAndMutate struct {
	CaseTable   bool
	Row         Row
	True, False []*btpb.Mutation
}

type ReadModifyWrite struct {
	CaseTable bool
	Row       Row
	Rules     []*btpb.ReadModifyWriteRule
}

// CreateTable with Families, or with one family, cf, that keeps one version when Families is nil. A Schema of nil
// creates the table with no row key schema.
type CreateTable struct {
	Schema   *adminpb.Type_Struct
	Families map[string]*adminpb.ColumnFamily
}

// UpdateTable with the mask row_key_schema. A Schema of nil clears it.
type SetRowKeySchema struct {
	Schema         *adminpb.Type_Struct
	IgnoreWarnings bool
}

// UpdateTable with the mask deletion_protection.
type SetDeletionProtection struct{ On bool }

// ModifyColumnFamilies with the modifications Mods, or one that creates the family cf2 when Mods is nil.
type ModifyColumnFamilies struct {
	Mods []*adminpb.ModifyColumnFamiliesRequest_Modification
}

// DropRowRange of every row, with its own deadline, past the case's. Production rejects a deadline under 2 minutes.
type DropRowRange struct{ Deadline time.Duration }

type DeleteTable struct{}

// GenerateConsistencyToken. The result leaves out the token, which differs between calls.
type GenerateConsistencyToken struct{}

// CheckConsistency with a token that no GenerateConsistencyToken returned.
type CheckConsistency struct{}

// ReadRows of one row, keeping each cell's raw bytes.
type ReadRow struct {
	CaseTable bool
	Row       Row
}

// ReadRows over the case's table, keeping only the row keys.
type ReadRowKeys struct{}

// SampleRowKeys over the case's table. The result leaves out the sample, which depends on how the server splits the
// table.
type SampleRowKeys struct{}

// GetTable with SCHEMA_VIEW.
type GetTable struct{}

// PrepareQuery of SQL, with Params as the declared query parameters. {table} in SQL stands for the case's table id.
// The run keeps the prepared query and its columns for the case's next ExecuteQuery.
type PrepareQuery struct {
	SQL    string
	Params map[string]*btpb.Type
}

// ExecuteQuery of the case's last prepared query, with Params as the parameter values. When the case has no prepared
// query, because it has called no PrepareQuery or its last one failed, it sends nothing and fails with the same status
// on every target. When the stream fails, the result keeps the rows and messages that came before the failure.
type ExecuteQuery struct {
	Params map[string]*btpb.Value
}

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

func CreateFamily(id string, cf *adminpb.ColumnFamily) *adminpb.ModifyColumnFamiliesRequest_Modification {
	return &adminpb.ModifyColumnFamiliesRequest_Modification{Id: id, Mod: &adminpb.ModifyColumnFamiliesRequest_Modification_Create{Create: cf}}
}

func UpdateFamily(id string, cf *adminpb.ColumnFamily) *adminpb.ModifyColumnFamiliesRequest_Modification {
	return &adminpb.ModifyColumnFamiliesRequest_Modification{Id: id, Mod: &adminpb.ModifyColumnFamiliesRequest_Modification_Update{Update: cf}}
}

func DropFamily(id string) *adminpb.ModifyColumnFamiliesRequest_Modification {
	return &adminpb.ModifyColumnFamiliesRequest_Modification{Id: id, Mod: &adminpb.ModifyColumnFamiliesRequest_Modification_Drop{Drop: true}}
}

// WithEmptyGCRules gives each of fs an empty GC rule, one with no rule in it, and returns fs.
func WithEmptyGCRules(fs map[string]*adminpb.ColumnFamily) map[string]*adminpb.ColumnFamily {
	for _, f := range fs {
		f.GcRule = &adminpb.GcRule{}
	}
	return fs
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

var (
	BytesType  = &btpb.Type{Kind: &btpb.Type_BytesType{BytesType: &btpb.Type_Bytes{}}}
	StringType = &btpb.Type{Kind: &btpb.Type_StringType{StringType: &btpb.Type_String{}}}
	Int64Type  = &btpb.Type{Kind: &btpb.Type_Int64Type{Int64Type: &btpb.Type_Int64{}}}
)

// A Param is a query parameter: its name, and its value with the value's type set. A Value with a type and no kind is
// NULL.
type Param struct {
	Name  string
	Value *btpb.Value
}

func BytesParam(name string, v []byte) Param {
	return Param{name, &btpb.Value{Type: BytesType, Kind: &btpb.Value_BytesValue{BytesValue: v}}}
}

func StringParam(name, v string) Param {
	return Param{name, &btpb.Value{Type: StringType, Kind: &btpb.Value_StringValue{StringValue: v}}}
}

func Int64Param(name string, n int64) Param {
	return Param{name, &btpb.Value{Type: Int64Type, Kind: &btpb.Value_IntValue{IntValue: n}}}
}

func NullParam(name string, t *btpb.Type) Param { return Param{name, &btpb.Value{Type: t}} }

// Query is a PrepareQuery of sql with the params' types, then an ExecuteQuery with their values.
func Query(sql string, params ...Param) []Call {
	types := map[string]*btpb.Type{}
	values := map[string]*btpb.Value{}
	for _, p := range params {
		types[p.Name] = p.Value.Type
		values[p.Name] = p.Value
	}
	return []Call{PrepareQuery{SQL: sql, Params: types}, ExecuteQuery{Params: values}}
}
