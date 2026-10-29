// ops_init_directives_show_content_test.go — the EXPLICIT content
// inspection path, end to end through the real command.
//
// WHY THIS FILE EXISTS
// --------------------
// Issue #59's operator decision (Session A, 2026-10-29) carries a
// constraint the original PR did not satisfy:
//
//	"Avoid displaying directive contents by default in cleanup reports;
//	 provide explicit inspection when needed."
//
// The PR's report printed a 96-character preview of every orphaned
// directive's content, by default, to stdout. A cleanup report is a LOG:
// it lands in a terminal scrollback and in CI output, and the directive
// text is the operator's own policy. So the default report now carries
// the candidate's identity and its dependencies, and the text is one
// explicit flag away.
//
// WHY THE END-TOEND LAYER
// -----------------------
// The unit tests pin the RENDERING. This pins the COMMAND: that the flag
// reaches the report, that the default really is quiet at the command
// line (not merely in a helper), and that an unknown flag is refused
// rather than ignored — because a flag that is silently ignored is how
// an operator ends up believing they inspected something they did not.
package main

import (
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/seed"
)

// armOrphanWorkspace sets up a workspace holding one de-registered
// directive, and arms the process-wide database singleton with it.
func armOrphanWorkspace(t *testing.T) {
	t.Helper()
	dm := orphanTestDM(t)
	insertSeededDirectiveRow(t, dm, "mpm-seed-removed-from-registry", "the de-registered policy text")
	installTestDM(t, dm)
}

// TestOpsInitDirectives_DefaultReportDoesNotPrintContent is the
// constraint, at the command line.
func TestOpsInitDirectives_DefaultReportDoesNotPrintContent(t *testing.T) {
	armOrphanWorkspace(t)

	code, out := runCLI(t, "ops", "init", "directives")
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "mpm-seed-removed-from-registry") {
		t.Errorf("the report does not name the candidate:\n%s", out)
	}
	if strings.Contains(out, "the de-registered policy text") {
		t.Errorf("the DEFAULT report printed the directive content:\n%s", out)
	}
	if !strings.Contains(out, "--show-content") {
		t.Errorf("the default report does not say how to inspect a candidate:\n%s", out)
	}
}

// TestOpsInitDirectives_ShowContentPrintsIt is the explicit inspection.
func TestOpsInitDirectives_ShowContentPrintsIt(t *testing.T) {
	armOrphanWorkspace(t)

	code, out := runCLI(t, "ops", "init", "directives", "--show-content")
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "the de-registered policy text") {
		t.Errorf("--show-content did not print the content:\n%s", out)
	}
	if !strings.Contains(out, "mpm-seed-removed-from-registry") {
		t.Errorf("--show-content did not name the candidate:\n%s", out)
	}
}

// TestOpsInitDirectives_UnknownFlagIsRefused — a flag that is silently
// ignored is how an operator comes to believe they inspected something
// they did not.
func TestOpsInitDirectives_UnknownFlagIsRefused(t *testing.T) {
	armOrphanWorkspace(t)

	code, out := runCLI(t, "ops", "init", "directives", "--prune")
	if code == 0 {
		t.Errorf("an unknown flag was accepted:\n%s", out)
	}
	if strings.Contains(out, "the de-registered policy text") {
		t.Errorf("a refused invocation still printed the content:\n%s", out)
	}
}

// TestOpsInitDirectives_NoPruneFlagExists — there is deliberately no
// automatic cleanup mechanism for directives.
//
// The policy question (#59) forbids one, and the report is the whole of
// what the policy authorises. A `--prune` flag would be an automatic
// cleanup mechanism introduced to close a policy question, so it must
// not exist — and the flag parser must not accept it.
func TestOpsInitDirectives_NoPruneFlagExists(t *testing.T) {
	armOrphanWorkspace(t)

	code, _ := runCLI(t, "ops", "init", "directives", "--prune")
	if code == 0 {
		t.Error("--prune was accepted; the policy forbids an automatic cleanup mechanism")
	}

	// And the row is still there, untouched.
	dm := getDB()
	if dm == nil {
		t.Fatal("no database")
	}
	var content string
	if err := dm.SQLDB().QueryRow(
		`SELECT content FROM memories WHERE id = 'mpm-seed-removed-from-registry'`).Scan(&content); err != nil {
		t.Fatalf("the candidate row is gone: %v", err)
	}
	if content != "the de-registered policy text" {
		t.Errorf("the candidate's content changed to %q", content)
	}
}

// TestOpsInitDirectives_ContentIsNotInAnyOtherReportSurface — the
// constraint is about the CONTENT, wherever the report puts it.
//
// The seed report itself prints identifiers, not text. This pins that
// the correction did not simply move the content somewhere else.
func TestOpsInitDirectives_ContentIsNotInAnyOtherReportSurface(t *testing.T) {
	armOrphanWorkspace(t)

	_, out := runCLI(t, "ops", "init", "directives")
	if strings.Count(out, "the de-registered policy text") != 0 {
		t.Errorf("the directive content appears in the default output:\n%s", out)
	}
	// The candidate is reported exactly once, by identity.
	if n := strings.Count(out, "mpm-seed-removed-from-registry"); n != 1 {
		t.Errorf("the candidate is named %d times, want 1:\n%s", n, out)
	}
}

// compile-time assertion that the report helper still takes the flag, so
// a future refactor cannot silently drop the explicit path.
var _ = func(orphans []seed.OrphanedDirective) { printOrphanedDirectives(orphans, false) }
