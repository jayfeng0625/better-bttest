// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// checkDropRowRangeDeadline returns production's error for a DropRowRange whose deadline is under 2 minutes, which
// production returns before it checks the table. Production's answer to a request with no deadline is unmeasured, so
// such a request passes.
func checkDropRowRangeDeadline(ctx context.Context, name string) error {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 2*time.Minute {
		return status.Errorf(codes.FailedPrecondition,
			"Insufficient deadline to delete a row range from %s. Please re-issue the request with a deadline at least 2m.", name)
	}
	return nil
}
