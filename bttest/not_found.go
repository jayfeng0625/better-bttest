// SPDX-License-Identifier: Apache-2.0

package bttest

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// tableNotFound is production's error for an RPC that names a table that does not exist.
func tableNotFound(name string) error {
	return status.Errorf(codes.NotFound, "Not found: %s", name)
}

// readNotFound is production's error for a DeleteTable of a table that does not exist, and for a GetMaterializedView
// of a view that does not exist. Production names the project by its number, in braces. The emulator has no project
// numbers, so it puts the project id in the braces.
func readNotFound(name string) error {
	project, rest, _ := strings.Cut(strings.TrimPrefix(name, "projects/"), "/")
	return status.Errorf(codes.NotFound, "Failed to read: projects/{%s}/%s", project, rest)
}
