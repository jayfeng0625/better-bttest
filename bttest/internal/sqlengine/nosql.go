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

var errNoSQL = status.Error(codes.Unimplemented, "the emulator was built without SQL. Use the better-bttest-with-sql image, or build with -tags googlesql")
