// SPDX-License-Identifier: Apache-2.0

package bttest_test

import (
	"context"
	"testing"

	"cloud.google.com/go/bigtable"
	"github.com/jayfeng0625/better-bttest/bttest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func newConn(t *testing.T) *grpc.ClientConn {
	t.Helper()
	srv, err := bttest.NewServer("localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func newInstanceAdmin(t *testing.T) *bigtable.InstanceAdminClient {
	t.Helper()
	iadmin, err := bigtable.NewInstanceAdminClient(context.Background(), "p", option.WithGRPCConn(newConn(t)))
	if err != nil {
		t.Fatal(err)
	}
	return iadmin
}

func wantStatus(t *testing.T, err error, code codes.Code, msg string) {
	t.Helper()
	if s, _ := status.FromError(err); s.Code() != code || s.Message() != msg {
		t.Errorf("error = %v, want %v %q", err, code, msg)
	}
}

// Production's error for a view that does not exist, as a probe of production recorded it on 2026-10-04.
func TestGetMissingMaterializedViewReturnsNotFound(t *testing.T) {
	_, err := newInstanceAdmin(t).MaterializedViewInfo(context.Background(), "i", "missing")
	wantStatus(t, err, codes.NotFound, "Failed to read: projects/{p}/instances/i/materializedViews/missing")
}

func TestCreateMaterializedViewStoresView(t *testing.T) {
	ctx := context.Background()
	iadmin := newInstanceAdmin(t)
	sent := bigtable.MaterializedViewInfo{
		MaterializedViewID: "v_expired",
		Query:              "SELECT _key AS rowKey FROM `items` WHERE mark['flag'] IS NULL ORDER BY rowKey",
		DeletionProtection: bigtable.Protected,
	}
	if err := iadmin.CreateMaterializedView(ctx, "i", &sent); err != nil {
		t.Fatalf("CreateMaterializedView: %v", err)
	}
	got, err := iadmin.MaterializedViewInfo(ctx, "i", "v_expired")
	if err != nil {
		t.Fatalf("MaterializedViewInfo: %v", err)
	}
	if *got != sent {
		t.Errorf("MaterializedViewInfo = %+v, want %+v", *got, sent)
	}
}

// Production's error for a view ID that is taken, as a probe of production recorded it on 2026-10-03.
func TestCreateMaterializedViewRejectsTakenID(t *testing.T) {
	ctx := context.Background()
	iadmin := newInstanceAdmin(t)
	first := bigtable.MaterializedViewInfo{MaterializedViewID: "v", Query: "SELECT _key FROM `items` ORDER BY _key"}
	if err := iadmin.CreateMaterializedView(ctx, "i", &first); err != nil {
		t.Fatalf("CreateMaterializedView: %v", err)
	}
	err := iadmin.CreateMaterializedView(ctx, "i", &bigtable.MaterializedViewInfo{MaterializedViewID: "v", Query: "SELECT _key FROM `other` ORDER BY _key"})
	wantStatus(t, err, codes.AlreadyExists, "Materialized View v already exists.")
	got, err := iadmin.MaterializedViewInfo(ctx, "i", "v")
	if err != nil {
		t.Fatalf("MaterializedViewInfo: %v", err)
	}
	if got.Query != first.Query {
		t.Errorf("MaterializedViewInfo query = %q, want the first view's %q", got.Query, first.Query)
	}
}
