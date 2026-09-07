package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildMpmForDYMTest compiles the mpm binary into a per-test temp dir.
// Mirrors the helpers in handlers_add_flag_order_test.go; duplicated
// locally so this test file stays self-contained.
func buildMpmForDYMTest(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "mpm")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", binPath, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mpm: %v\n%s", err, out)
	}
	return binPath
}

// runMpmDYM invokes the freshly built mpm against a disposable workspace
// and returns the user-facing output (logs filtered out). Exit code is
// returned alongside.
func runMpmDYM(t *testing.T, bin, workspace string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	cmd.Stdin, _ = os.Open(os.DevNull)
	out, err := cmd.CombinedOutput()
	output := filterDYMOutput(string(out))
	exit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			t.Fatalf("mpm %v: %v\n%s", args, err, out)
		}
	}
	return output, exit
}

// filterDYMOutput strips the slog log lines so the assertions focus on
// user-facing CLI output (recall results, hints, errors).
func filterDYMOutput(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "time=") || strings.HasPrefix(line, "level=") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestDidYouMean_CloseTypoSuggests verifies the core happy path:
// a single-token typo that is close (in Levenshtein distance) to a
// known command triggers a "did you mean?" hint after recall returns
// zero results.
func TestDidYouMean_CloseTypoSuggests(t *testing.T) {
	bin := buildMpmForDYMTest(t)
	workspace := t.TempDir()

	out, exit := runMpmDYM(t, bin, workspace, "lss")
	if exit != 0 {
		t.Fatalf("mpm lss should exit 0 on empty results, got %d\n%s", exit, out)
	}
	if !strings.Contains(out, "No memories found for: lss") {
		t.Errorf("expected recall message, got: %s", out)
	}
	if !strings.Contains(out, `Did you mean "ls"`) {
		t.Errorf("expected did-you-mean hint for ls, got: %s", out)
	}
	if !strings.Contains(out, "mpm ls --help") {
		t.Errorf("expected hint to suggest mpm ls --help, got: %s", out)
	}
}

// TestDidYouMean_UnrelatedTokenNoHint verifies the threshold boundary:
// a token that is NOT close to any known command name produces no
// "did you mean?" suggestion, but still gets a clear "Unknown command"
// line so the user knows the token didn't dispatch.
func TestDidYouMean_UnrelatedTokenNoHint(t *testing.T) {
	bin := buildMpmForDYMTest(t)
	workspace := t.TempDir()

	out, exit := runMpmDYM(t, bin, workspace, "pinoeques")
	if exit != 0 {
		t.Fatalf("mpm pinoeques should exit 0 on empty results, got %d\n%s", exit, out)
	}
	if !strings.Contains(out, "No memories found for: pinoeques") {
		t.Errorf("expected recall message, got: %s", out)
	}
	if strings.Contains(out, "Did you mean") {
		t.Errorf("did NOT expect hint for unrelated token, got: %s", out)
	}
	if !strings.Contains(out, `Unknown command "pinoeques"`) {
		t.Errorf("expected 'Unknown command' framing for unrelated token, got: %s", out)
	}
}

// TestDidYouMean_KnownCommandUnchanged pins the regression that known
// commands dispatch normally and never trigger the smart-recall / hint
// path. `mpm version` should print the version banner, period.
func TestDidYouMean_KnownCommandUnchanged(t *testing.T) {
	bin := buildMpmForDYMTest(t)
	workspace := t.TempDir()

	out, exit := runMpmDYM(t, bin, workspace, "version")
	if exit != 0 {
		t.Fatalf("mpm version should exit 0, got %d\n%s", exit, out)
	}
	if !strings.Contains(out, "MPM") {
		t.Errorf("expected version banner, got: %s", out)
	}
	if strings.Contains(out, "Did you mean") {
		t.Errorf("known command should never emit hint, got: %s", out)
	}
	if strings.Contains(out, "No memories found") {
		t.Errorf("known command should never invoke recall, got: %s", out)
	}
}

// TestDidYouMean_ExplicitRecallNoHint pins the second half of the
// invariant: when the user explicitly types `mpm recall <query>`, the
// hint path must NOT fire even if results are zero. The hint is a
// fallback affordance for ambiguous single-token input, not an add-on
// to the explicit recall surface.
func TestDidYouMean_ExplicitRecallNoHint(t *testing.T) {
	bin := buildMpmForDYMTest(t)
	workspace := t.TempDir()

	out, exit := runMpmDYM(t, bin, workspace, "recall", "lss")
	if exit != 0 {
		t.Fatalf("mpm recall lss should exit 0 on empty results, got %d\n%s", exit, out)
	}
	if !strings.Contains(out, "No memories found for: lss") {
		t.Errorf("expected recall empty-results message, got: %s", out)
	}
	if strings.Contains(out, "Did you mean") {
		t.Errorf("explicit recall should NOT emit did-you-mean hint, got: %s", out)
	}
	if strings.Contains(out, "Unknown command") {
		t.Errorf("explicit recall should NOT emit 'Unknown command', got: %s", out)
	}
}

// TestDidYouMean_TypoForRecallFiltered verifies the built-in-command
// filter: a token that is closest to "recall" (e.g., "recoll") should
// NOT suggest "recall" because recall is the path the smart-recall
// fallback has already routed through.
func TestDidYouMean_TypoForRecallFiltered(t *testing.T) {
	bin := buildMpmForDYMTest(t)
	workspace := t.TempDir()

	out, exit := runMpmDYM(t, bin, workspace, "recoll")
	if exit != 0 {
		t.Fatalf("mpm recoll should exit 0, got %d\n%s", exit, out)
	}
	if !strings.Contains(out, "No memories found for: recoll") {
		t.Errorf("expected recall empty-results message, got: %s", out)
	}
	if strings.Contains(out, `Did you mean "recall"`) {
		t.Errorf("should NOT suggest 'recall' (smart-recall target), got: %s", out)
	}
}

// TestDidYouMean_MultiTokenBypassesHint verifies that multi-token input
// does NOT trigger the smart-recall / hint path. `mpm lss foo` should
// behave like an unknown command rather than a recall query.
func TestDidYouMean_MultiTokenBypassesHint(t *testing.T) {
	bin := buildMpmForDYMTest(t)
	workspace := t.TempDir()

	out, _ := runMpmDYM(t, bin, workspace, "lss", "foo")
	if strings.Contains(out, "Did you mean") {
		t.Errorf("multi-token input should not trigger hint, got: %s", out)
	}
	if strings.Contains(out, "No memories found") {
		t.Errorf("multi-token input should not invoke recall, got: %s", out)
	}
}

// TestDidYouMean_SuggestPureFunction exercises the pure helper directly
// without the subprocess pipeline. This pins the algorithm and is fast
// enough to run as part of `go test`.
func TestDidYouMean_SuggestPureFunction(t *testing.T) {
	router := NewRouter()

	cases := []struct {
		token    string
		wantName string // "" means no suggestion expected
	}{
		{"lss", "ls"},
		{"statts", "stats"},
		{"addd", "add"},
		{"remembr", "remember"},
		{"pinoeques", ""}, // unrelated, no match
		{"recoll", ""},    // filtered (recall is the smart-recall target)
		{"", ""},          // empty token
	}
	for _, tc := range cases {
		t.Run(tc.token, func(t *testing.T) {
			got := suggestCommandName(tc.token, router)
			if tc.wantName == "" {
				if got != "" {
					t.Errorf("suggestCommandName(%q) = %q, want no suggestion", tc.token, got)
				}
			} else if got != tc.wantName {
				t.Errorf("suggestCommandName(%q) = %q, want %q", tc.token, got, tc.wantName)
			}
		})
	}
}

// TestLevenshtein verifies the bounded edit distance implementation
// against known fixtures. Catches algorithmic regressions early.
func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"kitten", "sitting", 3},
		{"lss", "ls", 1},
		{"statts", "stats", 1},
		{"recall", "recorl", 2},
	}
	for _, tc := range cases {
		t.Run(tc.a+"_vs_"+tc.b, func(t *testing.T) {
			if got := levenshtein(tc.a, tc.b); got != tc.want {
				t.Errorf("levenshtein(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestDidYouMeanThreshold verifies the threshold function's bound logic.
func TestDidYouMeanThreshold(t *testing.T) {
	cases := []struct {
		tokenLen int
		want     int
	}{
		{0, 0},
		{1, 2}, // short token, minimum is 2
		{3, 2},
		{6, 2}, // 6/3 = 2
		{9, 3}, // 9/3 = 3
		{15, 4}, // capped at 4
	}
	for _, tc := range cases {
		if got := didYouMeanThreshold(tc.tokenLen); got != tc.want {
			t.Errorf("didYouMeanThreshold(%d) = %d, want %d", tc.tokenLen, got, tc.want)
		}
	}
}
