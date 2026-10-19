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

// Without the googlesql tag, the emulator serves no SQL and names the image that does, and stores a view unchecked.
func TestSQLReturnsUnimplementedWithoutTheTag(t *testing.T) {
	ctx := context.Background()
	conn := newConn(t)
	client, err := bigtable.NewClient(ctx, "p", "i", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	iadmin, err := bigtable.NewInstanceAdminClient(ctx, "p", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	const want = "the emulator was built without SQL. Use the better-bttest-with-sql image, or build with -tags googlesql"
	_, err = client.PrepareStatement(ctx, "SELECT _key FROM t", nil)
	wantStatus(t, err, codes.Unimplemented, want)
	_, err = client.PrepareStatement(ctx, "SELECT _key FROM t WHERE @f > 0", map[string]bigtable.SQLType{"f": bigtable.Float64SQLType{}})
	wantStatus(t, err, codes.Unimplemented, want)
	// Table t does not exist, so a checked create would fail.
	sent := bigtable.MaterializedViewInfo{MaterializedViewID: "v", Query: "SELECT _key FROM t ORDER BY _key", DeletionProtection: bigtable.Unprotected}
	if err := iadmin.CreateMaterializedView(ctx, "i", &sent); err != nil {
		t.Fatalf("CreateMaterializedView: %v", err)
	}
	got, err := iadmin.MaterializedViewInfo(ctx, "i", "v")
	if err != nil {
		t.Fatalf("MaterializedViewInfo: %v", err)
	}
	if *got != sent {
		t.Errorf("MaterializedViewInfo = %+v, want %+v", *got, sent)
	}
}

// Without the googlesql tag, a stored view records no source table, so DeleteTable deletes a table the view names.
func TestDeleteTableIgnoresViewsWithoutTheTag(t *testing.T) {
	ctx := context.Background()
	conn := newConn(t)
	admin, err := bigtable.NewAdminClient(ctx, "p", "i", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	iadmin, err := bigtable.NewInstanceAdminClient(ctx, "p", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.CreateTable(ctx, "t"); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	view := bigtable.MaterializedViewInfo{MaterializedViewID: "v", Query: "SELECT _key FROM t ORDER BY _key", DeletionProtection: bigtable.Unprotected}
	if err := iadmin.CreateMaterializedView(ctx, "i", &view); err != nil {
		t.Fatalf("CreateMaterializedView: %v", err)
	}
	if err := admin.DeleteTable(ctx, "t"); err != nil {
		t.Errorf("DeleteTable: %v, want nil", err)
	}
}
