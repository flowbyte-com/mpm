package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestServeBootsAndPingResponds(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)
	t.Setenv("MPM_TELEMETRY_SOCKET", filepath.Join(tmp, "custom.sock"))
	t.Setenv("MPM_TELEMETRY_DB", filepath.Join(tmp, "custom.db"))

	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	go func() {
		if err := runServe([]string{}); err != nil && serveCtx.Err() == nil {
			t.Errorf("runServe: %v", err)
		}
	}()
	defer cancelServe()

	// Wait for socket.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(os.Getenv("MPM_TELEMETRY_SOCKET")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	out, err := exec.Command("go", "run", "github.com/flowbyte-com/mpm/cmd/mpm-telemetry", "ping").CombinedOutput()
	if err != nil {
		t.Fatalf("ping: %v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{`"collector_version"`, `"protocol_version"`, `"schema_version":`, `"queue_depth":`} {
		if !strings.Contains(s, want) {
			t.Errorf("ping output missing %q in:\n%s", want, out)
		}
	}
}
