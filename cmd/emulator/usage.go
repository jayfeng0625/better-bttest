// SPDX-License-Identifier: Apache-2.0

// The file name sorts after cbtemulator.go, so go doc shows upstream's package
// comment, and its synopsis, before the package comment below.

/*
Usage:

	emulator [flags]

Run emulator -h to list every flag. The fork adds one:

	-probe address
		Check that an emulator answers at address, given as host:port. The
		command asks the emulator to list its tables, and exits with status 0
		if it answers within 5 seconds, or 1 if it does not. The image's
		healthcheck runs -probe localhost:8086.

To start the emulator from a checkout the way the image does, listening on
port 8086 on every network interface, run:

	go run ./cmd/emulator -host 0.0.0.0 -port 8086
*/
package main
