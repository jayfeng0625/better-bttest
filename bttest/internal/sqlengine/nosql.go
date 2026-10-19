// SPDX-License-Identifier: Apache-2.0

//go:build !googlesql

package sqlengine

import (
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Prepare returns Unimplemented. The planner builds only with the googlesql tag, which the better-bttest-with-sql
// image sets, since go-googlesql links every one of its symbols once any is imported.
func Prepare(sql string, tables []Table, paramTypes map[string]*btpb.Type) (*Query, error) {
	return nil, errNoSQL
}

// CheckView accepts any query and compiles none, so a view is stored unchecked and blocks no DeleteTable.
func CheckView(sql string, tables []Table) (*Query, error) {
	return nil, nil
}

func CurrentView(v *Query, tables []Table) (*Query, error) {
	return nil, errNoSQL
}

var errNoSQL = status.Error(codes.Unimplemented, "the emulator was built without SQL. Use the better-bttest-with-sql image, or build with -tags googlesql")
