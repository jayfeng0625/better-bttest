// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jayfeng0625/better-bttest/bttest"
)

func TestProbeSucceedsWhenTheEmulatorServes(t *testing.T) {
	srv, err := bttest.NewServer("localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	if err := probe(context.Background(), srv.Addr); err != nil {
		t.Errorf("probe(%q) = %v, want nil", srv.Addr, err)
	}
}

func TestProbeFailsWhenNothingServes(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := probe(ctx, addr); err == nil {
		t.Errorf("probe(%q) = nil, want an error", addr)
	}
}
