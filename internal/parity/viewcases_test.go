// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"context"
	"testing"
)

// Each view case's Setup succeeds on the gate.
func TestViewCasesRunOnTheGate(t *testing.T) {
	_, target := startGate(t)
	for i, c := range ViewCases() {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), deadline(c))
			defer cancel()
			if _, err := Run(ctx, target, testRunID, i+1, c); err != nil {
				t.Fatal(err)
			}
		})
	}
}
