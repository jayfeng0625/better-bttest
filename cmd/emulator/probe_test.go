// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"testing"

	"github.com/jayfeng0625/better-bttest/bttest"
)

func TestProbeExitsZeroWhenTheEmulatorServes(t *testing.T) {
	srv, err := bttest.NewServer("localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	if got := runProbe(srv.Addr); got != 0 {
		t.Errorf("runProbe(%q) = %d, want 0", srv.Addr, got)
	}
}

func TestProbeExitsOneWhenNothingServes(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()

	if got := runProbe(addr); got != 1 {
		t.Errorf("runProbe(%q) = %d, want 1", addr, got)
	}
}
