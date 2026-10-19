// SPDX-License-Identifier: Apache-2.0

// Package sqlengine runs Bigtable's GoogleSQL queries over one table. The planner builds only with the googlesql
// tag. It parses and types each query with go-googlesql against a catalog built from the table's families, and
// compiles the resolved tree into Go operators that read rows through a Source. Without the tag, Prepare returns
// Unimplemented.
package sqlengine

import (
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Family is one column family as SQL sees it. Int64 marks a Sum, Min or Max family, whose values SQL types as
// INT64.
type Family struct {
	Name  string
	Int64 bool
}

// Table is one table or materialized view as SQL sees it. SELECT * lists a table's families in the order given.
type Table struct {
	Name     string
	Families []Family
	// View is a materialized view as CheckView compiled it. A view has no families.
	View *View
}

// View is a materialized view's query and its compiled plan. It is safe for concurrent use.
type View struct {
	sql  string
	plan atomic.Pointer[viewPlan]
}

// viewPlan is a view's compiled query and the source table's families it compiled against.
type viewPlan struct {
	query    *Query
	families []Family
}

// Table is the table the view's query reads.
func (v *View) Table() string { return v.plan.Load().query.Table }

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
	// View is the materialized view the query reads, or empty.
	View string
	// Families are the families the query reads.
	Families []string
	// Params are the declared parameters' types, by their declared name.
	Params map[string]Type

	root   scan
	outIDs []int32
	nSlots int
	// layout is how a materialized view stores the query's rows. Only CheckView sets it.
	layout *viewLayout
}

// scanCol is one column a table scan reads: the slot it fills and the column's index in the table.
type scanCol struct {
	id    int32
	index int
}

func unsupported(what string) error {
	return status.Errorf(codes.InvalidArgument, "The emulator does not support %s", what)
}
