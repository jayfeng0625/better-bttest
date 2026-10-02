// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The distroless image has no shell or probe binary, so a container
// healthcheck runs the emulator binary in probe mode.
var probeAddr = flag.String("probe", "", "address:port of an emulator to probe; exits 0 when it serves and 1 when it does not")

const probeTimeout = 5 * time.Second

// runProbe probes addr and returns the process exit code.
func runProbe(addr string) int {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if err := probe(ctx, addr); err != nil {
		fmt.Fprintf(os.Stderr, "probe %s: %v\n", addr, err)
		return 1
	}
	return 0
}

func probe(ctx context.Context, addr string) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	client := btapb.NewBigtableTableAdminClient(conn)
	_, err = client.ListTables(ctx, &btapb.ListTablesRequest{Parent: "projects/probe/instances/probe"})
	return err
}
