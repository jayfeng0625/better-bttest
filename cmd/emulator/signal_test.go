// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test re-executes its own binary with this variable set, and the child runs main.
const runMainEnv = "BETTER_BTTEST_RUN_MAIN"

func TestEmulatorShutsDownCleanlyOnSIGTERM(t *testing.T) {
	if os.Getenv(runMainEnv) == "1" {
		os.Args = []string{"emulator", "-port", "0"}
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestEmulatorShutsDownCleanlyOnSIGTERM$")
	cmd.Env = append(os.Environ(), runMainEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	timer := time.AfterFunc(30*time.Second, func() { cmd.Process.Kill() })
	defer timer.Stop()

	lines := bufio.NewScanner(stdout)
	for lines.Scan() && !strings.Contains(lines.Text(), "Cloud Bigtable emulator running on") {
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var rest strings.Builder
	for lines.Scan() {
		rest.WriteString(lines.Text())
		rest.WriteByte('\n')
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}

	if err := cmd.Wait(); err != nil {
		t.Errorf("emulator after SIGTERM: %v, want exit 0", err)
	}
	if !strings.Contains(rest.String(), "is now shutting down") {
		t.Errorf("emulator output after SIGTERM = %q, want the shutdown line", rest.String())
	}
}
