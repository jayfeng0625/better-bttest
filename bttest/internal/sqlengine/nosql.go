// SPDX-License-Identifier: Apache-2.0

//go:build !googlesql

package sqlengine

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Prepare returns Unimplemented. The planner builds only with the googlesql tag, which the better-bttest-with-sql
// image sets, since go-googlesql links every one of its symbols once any is imported.
func Prepare(sql string, tables []Table, params map[string]Type) (*Query, error) {
	return nil, errNoSQL
}

func PrepareView(sql string, tables []Table) (*Query, error) {
	return nil, errNoSQL
}

// CheckView accepts any query and returns no table, so a view is stored unchecked and blocks no DeleteTable.
func CheckView(sql string, tables []Table) (string, error) {
	return "", nil
}

var errNoSQL = status.Error(codes.Unimplemented, "the emulator was built without SQL. Use the better-bttest-with-sql image, or build with -tags googlesql")
