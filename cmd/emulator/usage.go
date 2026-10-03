// SPDX-License-Identifier: Apache-2.0

/*
Usage:

	emulator [flags]

The flags are:

	-host host
		The address to bind to.
	-port port
		The port to bind to.
	-address address
		An address:port or a unix socket path to listen on. It overrides
		-host and -port.
	-probe address
		Probe the emulator at address:port and exit: 0 when it lists its
		tables, and 1 when it does not. The image's healthcheck runs
		-probe localhost:8086.

From a checkout, this serves on port 8086 of every interface, as the image
does:

	go run ./cmd/emulator -host 0.0.0.0 -port 8086
*/
package main
