// SPDX-License-Identifier: Apache-2.0

//go:build !googlesql

package bttest_test

import (
	"context"
	"testing"

	"cloud.google.com/go/bigtable"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
)

// Without the googlesql tag, the emulator serves no SQL, and names the image that does.
func TestSQLReturnsUnimplementedWithoutTheTag(t *testing.T) {
	ctx := context.Background()
	conn := newConn(t)
	client, err := bigtable.NewClient(ctx, "p", "i", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.PrepareStatement(ctx, "SELECT _key FROM t", nil)
	wantStatus(t, err, codes.Unimplemented, "the emulator was built without SQL. Use the better-bttest-with-sql image, or build with -tags googlesql")
}
