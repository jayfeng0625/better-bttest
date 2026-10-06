// SPDX-License-Identifier: Apache-2.0

package parity

import (
	"context"
	"flag"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The smallest gcloud image that ships Google's stock emulator.
const stockImage = "gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators"

// Each stock container carries the run id under this label.
const runLabel = "better-bttest-parity.run"

var stockCommand = []string{"gcloud", "beta", "emulators", "bigtable", "start", "--host-port=0.0.0.0:8086"}

var stock = flag.Bool("stock", false, "with -real, run the cases on Google's stock emulator in Docker too, and report how it differs from the real table")

// TestStock runs every case on Google's stock emulator, and logs each case whose results differ from the real table's.
// A difference does not fail the test. A case that stops the emulator logs its panic, and the next case gets a fresh
// emulator.
func TestStock(t *testing.T) {
	if !*stock || *realInstance == "" {
		t.Skip("run with -stock -real=<project>/<instance>")
	}
	cases := Cases()
	runID := NewRunID(time.Now())
	real := realResults(t, cases, runID)
	label := runLabel + "=" + runID
	t.Cleanup(func() { removeContainers(t, label) })

	start := func() (Container, func()) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		c, stop, err := StartContainer(ctx, stockImage, stockCommand, label)
		if err != nil {
			t.Fatal(err)
		}
		return c, stop
	}
	emulator, stop := start()
	defer func() { stop() }()

	var differs, exits []string
	for i, c := range cases {
		want, ok := real[c.Name]
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), caseDeadline)
		got, err := Run(ctx, emulator.Target, runID, i+1, c)
		cancel()
		if log, stopped := emulator.Stopped(); stopped {
			t.Logf("The stock emulator stopped during %s:\n%s", c.Name, log)
			exits = append(exits, c.Name)
			stop()
			emulator, stop = start()
			continue
		}
		if err != nil {
			t.Logf("%s: %v", c.Name, err)
			differs = append(differs, c.Name)
			continue
		}
		if d := Diff(want, Normalize(emulator.Instance, runID, got)); d != "" {
			t.Logf("%s differs from the real table:\n%s", c.Name, d)
			differs = append(differs, c.Name)
		}
	}
	matched := len(real) - len(differs) - len(exits)
	t.Logf("stock matches %d of %d cases. %d differ, and %d stop the emulator.", matched, len(real), len(differs), len(exits))
	if len(exits) > 0 {
		t.Log("Cases that stop it:\n" + strings.Join(exits, "\n"))
	}
}

func removeContainers(t *testing.T, label string) {
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label="+label).Output()
	if err != nil {
		t.Error(err)
		return
	}
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		if err := exec.Command("docker", append([]string{"rm", "-f"}, ids...)...).Run(); err != nil {
			t.Error(err)
		}
	}
}
