// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
)

// A Result is one call's outcome. Only the run builds a Result, and the call's type sets which fields it fills:
//
//	every call       Call, Status
//	MutateRows       Entries
//	CheckAndMutate   Matched
//	ReadRow          Cells
//	ReadRowKeys      Keys
//	GetTable         Table
//	PrepareQuery     Columns
//	ExecuteQuery     Rows, Messages
//	GetView          View
//	ReadView         ViewRows
type Result struct {
	Call     string
	Status   Status
	Entries  []Status
	Matched  *bool
	Cells    []Cell
	Keys     []Hex
	Table    *TableView
	Columns  []Column
	Rows     [][]*btpb.Value
	Messages []string
	View     *ViewInfo
	ViewRows []ViewRow
}

// A Status is a gRPC status.
type Status struct {
	Code    codes.Code
	Message string
}

// A Cell is one stored cell, as raw bytes.
type Cell struct {
	Column string // family:qualifier
	TS     int64
	Value  Hex
}

// Hex is a row key or a cell value, as raw bytes.
type Hex []byte

// A TableView is a table as GetTable with SCHEMA_VIEW returns it.
type TableView struct {
	ColumnFamilies     map[string]*adminpb.ColumnFamily
	RowKeySchema       *adminpb.Type_Struct
	DeletionProtection bool
}

// A Column is one column of a prepared query's result, as PrepareQuery returns it.
type Column struct {
	Name string
	Type *btpb.Type
}

// A ViewInfo is a materialized view as GetMaterializedView returns it.
type ViewInfo struct {
	Query              string
	DeletionProtection bool
}

// A ViewRow is one row of a ReadRows over a view: its key, and its cells as raw bytes.
type ViewRow struct {
	Key   Hex
	Cells []Cell
}
