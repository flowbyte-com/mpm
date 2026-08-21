package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestHelpListsAllSubcommands(t *testing.T) {
	out, err := exec.Command("go", "run", "github.com/flowbyte-com/mpm/cmd/mpm-telemetry", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("--help failed: %v\n%s", err, out)
	}
	for _, sub := range []string{"serve", "ping", "query", "cost", "observe"} {
		if !strings.Contains(string(out), sub) {
			t.Errorf("--help missing subcommand %q in output:\n%s", sub, out)
		}
	}
}
