// SPDX-License-Identifier: Apache-2.0

// The file name sorts after cbtemulator.go, so go doc shows upstream's package
// comment, and its synopsis, before this comment.

/*
Usage:

	emulator [flags]

Run emulator -h to list every flag. The fork adds one:

	-probe address
		Probe the emulator at address:port, then exit with status 0 if it
		lists its tables, or 1 if it does not. The image's healthcheck runs
		-probe localhost:8086.

To serve on port 8086 of every interface from a checkout, as the image does,
run:

	go run ./cmd/emulator -host 0.0.0.0 -port 8086
*/
package main
