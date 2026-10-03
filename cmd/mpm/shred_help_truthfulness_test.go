// shred_help_truthfulness_test.go — MPM's shred surfaces must not claim
// erasure MPM does not perform.
//
// # The defect this pins
//
// The CLI described shredding as "secure delete". The implementation has
// never done that and cannot without machinery MPM deliberately lacks:
// ShredMemoryWithCascade issues a hard DELETE plus a defined cascade, and
// no shred path runs PRAGMA secure_delete, VACUUM, or wal_checkpoint. The
// content demonstrably survives in the audit log, in mirror history and its
// rotations, in backups, and in the freed pages of the DB file itself (see
// internal/core/shred_contract_test.go for the measurement).
//
// The risk is a promise, not a bug. An operator who reads "secure delete"
// and then shreds a memory containing a credential has been told something
// MPM cannot deliver. So these tests assert the *negative*: the retired
// vocabulary stays retired, and each surface says what it actually does.
//
// Two related mismatches are pinned here because they are the same class of
// lie — a verb whose name promises more than the code does:
//
//   - `mpm session shred` and `mpm reference shred` are SOFT deletes
//     (deleted_at), reversible, despite the shred verb.
//   - `dm.ShredSkill` is a soft delete; only PermanentlyShredSkill is hard.
//
// No live state is touched: every test here is a pure string assertion over
// help text, plus handler calls against a temp workspace.

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestShredHelp_MakesNoErasureClaim scans the user-facing help sources for
// the phrase that started this. It is a source scan rather than a rendered
// output comparison because the strings are spread across several render
// styles (plain fmt.Println, the shared render package, and the render
// package's Label/Section calls), and the point is the claim, not the layout.
func TestShredHelp_MakesNoErasureClaim(t *testing.T) {
	sources := []string{
		"handlers_shred.go",
		"handlers_session.go",
		"handlers_topic.go",
		"handlers_lesson.go",
		"handlers_handoff.go",
		"handlers_cognitive_verbs.go",
		"router.go",
		"simple_cmds.go",
	}
	for _, name := range sources {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		for _, banned := range []string{"Secure delete", "secure delete"} {
			if idx := strings.Index(string(data), banned); idx >= 0 {
				line := lineOf(string(data), idx)
				t.Errorf("%s:%d still says %q.\n"+
					"  MPM does not perform secure deletion. Use the accurate scope:\n"+
					"  'remove from active state' for a real shred, or 'soft delete'\n"+
					"  where the operation sets deleted_at.", name, line, banned)
			}
		}
	}
}

// TestShredHelpPage_DisclosesTheLimit is the positive half: the shred help
// page must state the contract, not merely avoid the old wording. A page
// that merely says "delete" is still ambiguous between "gone" and "removed
// from active state", and ambiguity is how the original claim survived.
func TestShredHelpPage_DisclosesTheLimit(t *testing.T) {
	out := captureShredHelp(t)

	required := []struct{ want, why string }{
		{"Remove objects from active MPM state", "the help title must name the actual scope"},
		{"does not erase bytes", "the boundary must be stated, not implied"},
		{"mirror.jsonl", "mirror history survives a shred and must be disclosed"},
		{"database backups", "backups survive a shred and must be disclosed"},
		{"WAL", "the WAL and free pages survive a shred and must be disclosed"},
		{"no secure-erasure capability", "MPM has no erasure facility; say so"},
	}
	for _, r := range required {
		if !strings.Contains(out, r.want) {
			t.Errorf("shred help does not contain %q — %s.\nGot:\n%s", r.want, r.why, out)
		}
	}
}

// TestShredHelpPage_PrimaryPathIsDocumented covers a plain omission: the
// per-ID memory shred is the primary and best-supported form of the
// operation, and it was not listed on the help page at all, while the six
// bulk forms — which cannot currently be confirmed — were.
func TestShredHelpPage_PrimaryPathIsDocumented(t *testing.T) {
	out := captureShredHelp(t)
	if !strings.Contains(out, "mpm shred <id>") {
		t.Errorf("shred help does not document `mpm shred <id>`, the per-object form.\nGot:\n%s", out)
	}
}

// TestBulkShredConfirmation_IsHonest pins the force-flag finding.
//
// router.parseFlags consumes -f/--force from argv and republishes it as
// MPM_FORCE=1 (router.go:508). The bulk shred handlers each scan their own
// args for the literal token, so they never see it: every bulk form aborts
// at its confirmation prompt regardless of what the operator types.
//
// The handlers previously told the operator to re-run the exact command
// that had just failed. That is the worst possible failure mode for a
// destructive command — it sends someone looking for a "real" invocation
// that does not exist. The fix here is the message, not the flag plumbing:
// rewiring it would activate six currently-unreachable bulk deletes
// (including `shred database`, which os.Remove()s the DB file and recreates
// it with no cascade and no backup handling), which is a behavior change
// belonging in its own change with its own gates.
func TestBulkShredConfirmation_IsHonest(t *testing.T) {
	data, err := os.ReadFile("handlers_shred.go")
	if err != nil {
		t.Fatalf("read handlers_shred.go: %v", err)
	}
	src := string(data)

	if strings.Contains(src, "to confirm.") && !strings.Contains(src, "Nothing was deleted") {
		t.Errorf("a bulk shred confirmation warning still instructs the operator to " +
			"re-run a command that cannot work (no 'Nothing was deleted' disclosure)")
	}
	if !strings.Contains(src, "Nothing was deleted.") {
		t.Errorf("bulk shred warnings do not disclose that nothing was deleted; " +
			"a destructive command must say so when it refuses")
	}
	// The help page must not advertise the bulk forms as confirmable.
	out := captureShredHelp(t)
	if strings.Contains(out, "requires -f to confirm") {
		t.Errorf("shred help still advertises `-f` confirmation for the bulk forms, "+
			"which the router consumes before the handlers see it:\n%s", out)
	}
}

// TestSessionShred_AdvertisesItsRealBehavior pins the second class of lie:
// `mpm session shred <id>` calls store.DeleteMemory, which sets deleted_at.
// It is reversible. Calling it "secure delete" was wrong twice over.
func TestSessionShred_AdvertisesItsRealBehavior(t *testing.T) {
	data, err := os.ReadFile("handlers_session.go")
	if err != nil {
		t.Fatalf("read handlers_session.go: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, "Soft-delete session") {
		t.Errorf("mpm session help does not say that `session shred` is a soft delete; " +
			"it sets deleted_at and is recoverable, which the shred verb conceals")
	}
}

// lineOf returns the 1-indexed line number containing byte offset idx.
func lineOf(src string, idx int) int {
	return 1 + strings.Count(src[:idx], "\n")
}

// captureShredHelp renders the shred help page and returns it. `respond`
// writes straight to os.Stdout, so this uses the same os.Pipe capture the
// other handler tests in this package use.
func captureShredHelp(t *testing.T) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = orig
		_ = r.Close()
		_ = w.Close()
	})

	code := handleShred([]string{"--help"})
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	if code != 0 {
		t.Fatalf("handleShred --help returned %d, want 0", code)
	}
	if buf.Len() == 0 {
		t.Fatalf("shred help produced no output")
	}
	return buf.String()
}
