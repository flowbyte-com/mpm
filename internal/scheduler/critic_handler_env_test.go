// critic_handler_env_test.go — the critic subprocess must audit the SAME
// substrate the scheduler dispatches against.
//
// THE DEFECT THIS PINS
//
// The critic-audit handler passed MPM_DB_PATH=defaultDBPath() to the
// child and nothing else. The child therefore resolved ITS workspace
// from the inherited MPM_WORKSPACE — the scheduler's ENVIRONMENT
// workspace — while the scheduler itself may have been pointed at
// another workspace with --db.
//
// The result: the scheduler dispatched wakes against one substrate and
// the critic audited another. The critic's findings were about a
// database the scheduler was not serving, and (before the read-only
// adjudication) its writes landed there too.
//
// THE CONTRACT (D4)
//
// The workspace AND the database are pinned for the child. The child's
// own resolver requires the two to agree, so a child that somehow
// resolved a different workspace refuses to start rather than auditing
// the wrong substrate.

package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCriticAuditHandler_PinsWorkspaceAndDatabase proves the child is
// given the resolved workspace and the resolved database — and NOT the
// scheduler's ambient environment.
func TestCriticAuditHandler_PinsWorkspaceAndDatabase(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "child-env.txt")
	// A stand-in for mpm-critic: it records the two selectors it was
	// given and exits 0.
	script := "#!/bin/sh\n" +
		"printf 'WS=%s\\nDB=%s\\n' \"$MPM_WORKSPACE\" \"$MPM_DB_PATH\" > " +
		shellQuote(dump) + "\n" +
		"exit 0\n"
	bin := filepath.Join(dir, "fake-critic.sh")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake critic: %v", err)
	}
	t.Setenv("MPM_CRITIC_BIN", bin)

	// The scheduler's ambient environment points somewhere ELSE — the
	// inherited MPM_WORKSPACE is what the old code leaked into the
	// child.
	t.Setenv("MPM_WORKSPACE", "/tmp/ambient-workspace")
	t.Setenv("MPM_DB_PATH", "/tmp/ambient-workspace/src/db/mpm.db")

	resolvedWS := t.TempDir()
	resolvedDB := filepath.Join(resolvedWS, "src", "db", "mpm.db")

	h := NewCriticAuditHandler(resolvedWS, resolvedDB)
	if err := h(context.Background(), Wake{}); err != nil {
		t.Fatalf("critic audit handler: %v", err)
	}

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the child did not run: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "WS="+resolvedWS) {
		t.Errorf("the child must receive the resolved workspace %q; got:\n%s", resolvedWS, got)
	}
	if !strings.Contains(got, "DB="+resolvedDB) {
		t.Errorf("the child must receive the resolved database %q; got:\n%s", resolvedDB, got)
	}
	if strings.Contains(got, "ambient-workspace") {
		t.Errorf("the child must NOT inherit the ambient workspace; got:\n%s", got)
	}
}

// shellQuote wraps s in single quotes for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
