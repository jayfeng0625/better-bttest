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

// readNotFound is production's error for a DeleteTable of a table that does not exist, and for an admin call on a
// materialized view that does not exist.
func readNotFound(name string) error {
	return status.Errorf(codes.NotFound, "Failed to read: %s", numberedProject(name))
}

// numberedProject writes a resource name as production writes it in a read error, with the project in braces.
// Production puts the project number there. The emulator has only the project ID, so it writes the ID.
func numberedProject(name string) string {
	project, rest, _ := strings.Cut(strings.TrimPrefix(name, "projects/"), "/")
	return "projects/{" + project + "}/" + rest
}
