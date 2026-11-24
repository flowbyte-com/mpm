package opencode

// R2 integration: the real Launch path with a fake runtime binary that
// prints a marker and fails. It must return a structured, secret-free
// LaunchError, never res,nil.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLaunchReturnsStructuredFailureWhenRunAndInspectFail(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := BuildTree(Spec{Root: root, Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	const marker = "SYNTHETIC-SECRET-MARKER"
	bin := filepath.Join(t.TempDir(), "fake-opencode")
	script := `#!/bin/sh
case "$*" in
  *"debug agents"*) echo '[{"id":"mpm-worker","mode":"primary","permissions":[]}]'; exit 0;;
  *"session.list"*) echo "` + marker + `" >&2; exit 9;;
  *"run"*) echo "` + marker + `" >&2; exit 7;;
esac
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "sidecar.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := NewRuntime(Config{
		Root: root, OpenCodeBin: bin, RepoPath: workspace,
		Model:         "mpm-sidecar/permitted-model",
		AllowedAgents: []string{"mpm-worker"},
		Sidecar:       SidecarConfig{SocketPath: sock},
		Timeout:       30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := rt.Launch(context.Background(), LaunchRequest{
		SessionID: "ses_r2_fake", Agent: "mpm-worker",
		Workspace: workspace, Title: "r2", Prompt: "test",
	})
	var le *LaunchError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want *LaunchError", err)
	}
	if le.RunFailure != LaunchExitFailure || le.ExitCode != 7 {
		t.Fatalf("run = %q/%d", le.RunFailure, le.ExitCode)
	}
	if le.InspectFailure != InspectUnavailable {
		t.Fatalf("inspect = %q", le.InspectFailure)
	}
	if le.StateObserved || !le.InferenceUnknown {
		t.Fatalf("state observed=%v inference unknown=%v", le.StateObserved, le.InferenceUnknown)
	}
	if le.OutputBytes == 0 || le.OutputSHA256 == "" {
		t.Fatalf("output evidence = %d/%q", le.OutputBytes, le.OutputSHA256)
	}
	if strings.Contains(le.Error(), marker) {
		t.Fatalf("the marker reached the durable diagnostic: %q", le.Error())
	}
	if res == nil || !strings.Contains(res.Output, marker) {
		t.Fatalf("the in-memory output was not captured for the digest: %+v", res)
	}
}
