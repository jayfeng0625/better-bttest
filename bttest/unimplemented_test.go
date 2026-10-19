// SPDX-License-Identifier: Apache-2.0

package bttest_test

import (
	"context"
	"testing"

	"cloud.google.com/go/bigtable"
	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
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

func wantStatus(t *testing.T, err error, code codes.Code, msg string) {
	t.Helper()
	if s, _ := status.FromError(err); s.Code() != code || s.Message() != msg {
		t.Errorf("error = %v, want %v %q", err, code, msg)
	}
}

// An RPC that the emulator does not serve returns Unimplemented, and the emulator keeps serving.
func TestUnservedCallsReturnUnimplemented(t *testing.T) {
	ctx := context.Background()
	conn := newConn(t)
	iadmin, err := bigtable.NewInstanceAdminClient(ctx, "p", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := bigtable.NewAdminClient(ctx, "p", "i", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	// The Go client has no change stream calls, so the data call uses the generated client.
	data := btpb.NewBigtableClient(conn)

	tests := []struct {
		name string
		call func() error
		want string
	}{
		{"ListLogicalViews", func() error {
			_, err := iadmin.LogicalViews(ctx, "i")
			return err
		}, "method ListLogicalViews not implemented"},
		{"ListBackups", func() error {
			_, err := admin.Backups(ctx, "c").Next()
			return err
		}, "method ListBackups not implemented"},
		{"GenerateInitialChangeStreamPartitions", func() error {
			stream, err := data.GenerateInitialChangeStreamPartitions(ctx, &btpb.GenerateInitialChangeStreamPartitionsRequest{TableName: "projects/p/instances/i/tables/t"})
			if err != nil {
				return err
			}
			_, err = stream.Recv()
			return err
		}, "method GenerateInitialChangeStreamPartitions not implemented"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantStatus(t, tc.call(), codes.Unimplemented, tc.want)
		})
	}
	if _, err := iadmin.MaterializedViewInfo(ctx, "i", "missing"); err == nil {
		t.Error("MaterializedViewInfo after the unserved calls succeeded, want NotFound")
	}
}
