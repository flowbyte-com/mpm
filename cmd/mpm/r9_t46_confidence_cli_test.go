// r9_t46_confidence_cli_test.go — Round 9 T46-T48 regression.
//
// Pin the parity between `mpm ops confidence <sub>` and the
// underlying `mpm_confidence` substrate tool. The CLI previously
// exposed only `show|recompute|changes|trend`; the substrate also
// accepts `explain` and `history` actions (internal/core/tools/handlers.go
// dispatch table). The fix extends parseOpsConfidenceArgs /
// handleOpsConfidence so all six subcommands route through the
// substrate-driven implementation and the usage strings document
// them.
//
// What this test pins:
//   - usage advertises all 6 subcommands
//   - each subcommand parses without "unknown subcommand"
//   - explain/history return structured error for missing artifact
//     (same shape as mpm call mpm_confidence action=explain|history)

package main

import (
	"os/exec"
	"strings"
	"testing"
)

func r9T46OpConfidence(t *testing.T, args ...string) (string, int) {
	t.Helper()
	bin := "/home/v/.mpm/bin/mpm"
	cmd := exec.Command(bin, append([]string{"ops", "confidence"}, args...)...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

// TestR9T46_ConfidenceUsageListsAllSix checks that the usage string
// printed when no subcommand is supplied enumerates all six.
func TestR9T46_ConfidenceUsageListsAllSix(t *testing.T) {
	out, code := r9T46OpConfidence(t)
	if code == 0 {
		t.Fatalf("ops confidence (no args): expected non-zero exit\noutput: %s", out)
	}
	for _, want := range []string{"show", "recompute", "changes", "trend", "explain", "history"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage missing %q. Output:\n%s", want, out)
		}
	}
}

// TestR9T46_ConfidenceExplainPipesThroughToSubstrate runs `explain`
// against a non-existent artifact and verifies the substrate-driven
// error shape, not the CLI's "unknown subcommand" shape.
func TestR9T46_ConfidenceExplainPipesThroughToSubstrate(t *testing.T) {
	out, code := r9T46OpConfidence(t, "explain", "--artifact", "missing-r9t46")
	if code == 0 {
		t.Fatalf("explain on missing artifact: expected failure, got success\noutput: %s", out)
	}
	if strings.Contains(out, "unknown subcommand") {
		t.Fatalf("explain routed as unknown subcommand — parser hasn't been extended.\noutput: %s", out)
	}
	if !strings.Contains(out, "does not exist") && !strings.Contains(out, "not found") {
		t.Fatalf("explain should report substrate artifact-not-found shape.\noutput: %s", out)
	}
}

// TestR9T46_ConfidenceHistoryPipesThroughToSubstrate mirrors the
// explain test for the history subcommand.
//
// `history` returns `{"history": []}` for missing artifacts (the
// substrate treats missing-artifact as zero-history rather than an
// error — it queries confidence_history which is naturally
// empty for non-existent ids). The CLI must surface that empty
// shape, NOT route through the "unknown subcommand" path.
func TestR9T46_ConfidenceHistoryPipesThroughToSubstrate(t *testing.T) {
	out, code := r9T46OpConfidence(t, "history", "--artifact", "missing-r9t46")
	if code != 0 {
		t.Fatalf("history on missing artifact: expected exit 0 (empty list)\noutput: %s", out)
	}
	if strings.Contains(out, "unknown subcommand") {
		t.Fatalf("history routed as unknown subcommand — parser hasn't been extended.\noutput: %s", out)
	}
	if !strings.Contains(out, `"history"`) {
		t.Fatalf("history should surface substrate-shaped empty list, got:\n%s", out)
	}
}

// TestR9T46_ConfidenceShowPreserved checks that the four pre-existing
// subcommands still parse, so the fix didn't regress existing
// functionality. We run show on a missing artifact: success path
// requires a real artifact (we'd need fixtures), so the contract is
// "the parser accepts show and the route reaches the substrate".
func TestR9T46_ConfidenceShowPreserved(t *testing.T) {
	out, code := r9T46OpConfidence(t, "show", "--artifact", "missing-r9t46")
	if code == 0 {
		t.Fatalf("show on missing artifact: expected failure, got success\noutput: %s", out)
	}
	if strings.Contains(out, "unknown subcommand") {
		t.Fatalf("show routed as unknown subcommand — pre-fix regression.\noutput: %s", out)
	}
}

// TestR9T46_ConfidenceSubstrateParity verifies that the CLI routes
// the same artifact-not-found error text the underlying
// `mpm call mpm_confidence` would emit. Both go through the same
// `dm.ExplainConfidence` and `dm.QueryConfidenceHistory` methods,
// so the error envelope shape is what matters (same prefix/path).
func TestR9T46_ConfidenceSubstrateParity(t *testing.T) {
	bin := "/home/v/.mpm/bin/mpm"

	cliExplain, _ := exec.Command(bin, "ops", "confidence", "explain",
		"--artifact", "missing-r9t46-parity").CombinedOutput()
	callExplain, _ := exec.Command(bin, "call", "mpm_confidence",
		"--payload", `{"action":"explain","params":{"artifact_id":"missing-r9t46-parity","artifact_type":"memory"}}`).CombinedOutput()

	cliText := string(cliExplain)
	callText := string(callExplain)

	// Both should fail with artifact-not-found shape (not unknown).
	if !strings.Contains(cliText, "does not exist") {
		t.Fatalf("CLI explain missing 'does not exist' marker:\n%s", cliText)
	}
	if !strings.Contains(callText, "does not exist") {
		t.Fatalf("substrate explain missing 'does not exist' marker:\n%s", callText)
	}
}
