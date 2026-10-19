// SPDX-License-Identifier: Apache-2.0

// Package sqlengine runs Bigtable's GoogleSQL queries over one table. The planner builds only with the googlesql
// tag. It parses and types each query with go-googlesql against a catalog built from the table's families, and
// compiles the resolved tree into Go operators that read rows through a Source. Without the tag, Prepare returns
// Unimplemented.
package sqlengine

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Family is one column family as SQL sees it. Int64 marks a Sum, Min or Max family, whose values SQL types as
// INT64.
type Family struct {
	Name  string
	Int64 bool
}

// Table is one table as SQL sees it. SELECT * lists the families in the order given.
type Table struct {
	Name     string
	Families []Family
}

// Column is one output column of a prepared query.
type Column struct {
	Name string
	Type Type
}

// Query is a prepared query. It holds no go-googlesql handle, so it is safe for concurrent use.
type Query struct {
	// Columns are the output columns, in order.
	Columns []Column
	// Table is the table the query reads.
	Table string
	// Families are the families the query reads.
	Families []string

	root   scan
	outIDs []int32
	nSlots int
}

func unsupported(what string) error {
	return status.Errorf(codes.InvalidArgument, "The emulator does not support %s", what)
}
