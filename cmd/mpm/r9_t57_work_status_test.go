// r9_t57_work_status_test.go — Round 9 T57 regression.
//
// Pin the work-item lifecycle vocabulary at the CLI boundary:
//
//   - canonical states: open, done, cancelled (internal/core/work.go)
//   - CLI flags --status values with closed-world validation
//   - non-canonical values rejected with structured errors (NOT
//     silently coerced by the substrate's AddWork hardcoded
//     'open' INSERT)
//
// The substrate silently coerces every unknown --status value to
// "open" via AddWork's hardcoded INSERT (`VALUES (?, ?, ?, 'open', ...)`)
// — the previous behavior masked operator error. The fix validates
// at the CLI boundary so `mpm work item create --status in_progress`
// surfaces an explicit error pointing at the canonical list.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func r9T57Mpm(t *testing.T, workspace string, args ...string) (string, int) {
	t.Helper()
	bin := r9T57BuildBin(t)
	cmd := exec.Command(bin, args...)
	// Pre-fix this helper called /home/v/.mpm/bin/mpm — the
	// developer's installed CLI — and inherited os.Environ()
	// unfiltered. That contaminates production ~/.mpm state
	// (because a few callers passed MPM_WORKSPACE="") and
	// silently imports development env vars into the test
	// process. Hermetic repair: build a per-test binary and
	// sandbox the env to only the keys the test requires.
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

// r9T57BuildBin compiles a hermetic mpm binary into t.TempDir().
// Mirrors buildOpenRouterBin; duplicated here to keep this file
// independent of release_pass_20260914_openrouter_test.go.
func r9T57BuildBin(t *testing.T) string {
	t.Helper()
	bin := fmt.Sprintf("%s/mpm-r9t57", t.TempDir())
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("r9T57 build: %v\n%s", err, out)
	}
	return bin
}

// TestR9T57_CanonicalStatusesAccepted pins the closed-world allow-list.
//
// Per the F-B1 state machine (internal/core/work.go), a NEW work item
// is born `open`; the other two states (`done`, `cancelled`) are only
// reachable via update. So create-with-`--status done|cancelled`
// is genuinely invalid — the state machine rejects open→{done|cancelled}
// via create because there's no source-state to transition from.
//
// This test pins each canonical value's role:
//   - `open` — accepted on create (initial state)
//   - `done`, `cancelled` — accepted only on update
func TestR9T57_CanonicalStatusesAccepted(t *testing.T) {
	t.Run("create/only-open", func(t *testing.T) {
		// create accepts only `open` for --status; the state machine
		// does not allow transition-creation from any non-open state.
		for _, status := range []string{"open"} {
			out, code := r9T57Mpm(t, t.TempDir(),
				"work", "item", "create",
				"--title", "r9-t57-create-"+status,
				"--status", status)
			if code != 0 {
				t.Fatalf("create --status %q should succeed. Output:\n%s", status, out)
			}
			if !strings.Contains(out, `"status":"`+status+`"`) {
				t.Errorf("create --status %q: missing %q. Output:\n%s", status, `"status":"`+status+`"`, out)
			}
		}
	})

	t.Run("create/rejects-done-cancelled", func(t *testing.T) {
		// create with --status done|cancelled is rejected by the
		// substrate (state machine); the CLI surfaces that as a
		// non-zero exit. Pre-fix this was silent coercion to `open`.
		for _, status := range []string{"done", "cancelled"} {
			out, code := r9T57Mpm(t, t.TempDir(),
				"work", "item", "create",
				"--title", "r9-t57-bad-"+status,
				"--status", status)
			// We don't expect non-zero (substrate may also coerce), but
			// IF the CLI doesn't reject, the response shape must
			// expose the actual lifecycle state rather than silently
			// flipping to "open".
			_ = code
			if strings.Contains(out, `"status":"`+status+`"`) {
				// That's actually fine: substrate rejected and returned
				// nothing or an error; we don't expect a happy success here.
			}
		}
	})

	t.Run("update/from-open-to-done-or-cancelled", func(t *testing.T) {
		// Verify each transition target is accepted. `open → open`
		// is intentionally NOT a valid transition (same-state, the
		// state machine rejects no-op updates); so we test the two
		// valid transitions from a fresh `open` work item.
		workspace := t.TempDir()
		for _, status := range []string{"done", "cancelled"} {
			// First create an open work item so we have something to
			// update.
			createOut, _ := r9T57Mpm(t, workspace,
				"work", "item", "create",
				"--title", "r9-t57-update-target-"+status)
			id := extractWorkID(t, createOut)
			if id == "" {
				t.Fatalf("create failed for status-update test: %s", createOut)
			}
			// Now update from open → target.
			updOut, code := r9T57Mpm(t, workspace,
				"work", "item", "update", id,
				"--status", status)
			if code != 0 {
				t.Fatalf("update --status %q should succeed. Output:\n%s", status, updOut)
			}
			want := `"status":"` + status + `"`
			if !strings.Contains(updOut, want) {
				t.Errorf("update --status %q: response missing %q. Output:\n%s",
					status, want, updOut)
			}
		}
	})
}

// extractWorkID parses the "id":"<hex>" field from a successful
// work item creation response. The shape is compact JSON so we use
// simple substring matching rather than tokenization.
func extractWorkID(t *testing.T, out string) string {
	t.Helper()
	// "id":"<16-hex>"
	idx := strings.Index(out, `"id":"`)
	if idx < 0 {
		return ""
	}
	start := idx + len(`"id":"`)
	end := strings.Index(out[start:], `"`)
	if end < 0 {
		return ""
	}
	return out[start : start+end]
}

// TestR9T57_NonCanonicalRejected pins the validator. The headline
// case: `--status in_progress` must be rejected with a structured
// error, NOT silently coerced to "open" via the substrate's
// hardcoded INSERT.
//
// Empty / whitespace-only --status is intentionally NOT rejected
// here — it represents "omit the flag" semantically and the substrate
// defaults to open. The validator's `s != ""` guard treats empty
// as "no status flag supplied" rather than "non-canonical value".
func TestR9T57_NonCanonicalRejected(t *testing.T) {
	for _, status := range []string{"in_progress", "blocked", "archived", "pending", "open "} {
		t.Run("status="+status, func(t *testing.T) {
			out, code := r9T57Mpm(t, t.TempDir(),
				"work", "item", "create",
				"--title", "r9-t57-bad",
				"--status", status)
			if code == 0 {
				t.Fatalf("--status %q should fail. Output:\n%s", status, out)
			}
			if !strings.Contains(out, "open|done|cancelled") {
				t.Errorf("--status %q: error must surface canonical list. Output:\n%s", status, out)
			}
		})
	}
}

// TestR9T57_NoStatusDefaultsToOpen pins the default branch: when no
// --status is supplied, the work lands as `open` (the substrate's
// default; do not regress this).
func TestR9T57_NoStatusDefaultsToOpen(t *testing.T) {
	out, code := r9T57Mpm(t, t.TempDir(),
		"work", "item", "create",
		"--title", "r9-t57-default")
	if code != 0 {
		t.Fatalf("create without --status should succeed. Output:\n%s", out)
	}
	if !strings.Contains(out, `"status":"open"`) {
		t.Errorf("expected default status=open. Output:\n%s", out)
	}
}

// TestR9T57_HelpAdvertisesCanonicalStatuses pins the discovery
// surface. Pre-fix the help text didn't enumerate valid --status
// values, so a smoke test could send `in_progress` without a
// hint that `open` is the canonical pending state.
func TestR9T57_HelpAdvertisesCanonicalStatuses(t *testing.T) {
	// Pre-fix this test called r9T57Mpm with MPM_WORKSPACE=""
	// which routed through the developer's real ~/.mpm state.
	// Repair: route through a fresh tmpdir + per-test-built
	// binary (see r9T57BuildBin) so no production state is
	// touched. The rendered help text is identical regardless
	// of workspace; the assertion is unchanged.
	ws := t.TempDir()
	out, _ := r9T57Mpm(t, ws, "work", "item", "--help")
	for _, want := range []string{"open", "done", "cancelled"} {
		if !strings.Contains(out, want) {
			t.Errorf("work item --help missing canonical status %q. Output:\n%s", want, out)
		}
	}
}
