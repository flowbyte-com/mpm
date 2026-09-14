// handlers_why_help_test.go — Regression coverage for the
// 2026-09-14 release-pass `mpm why --help` short-circuit.
//
// Prior to the fix, `mpm why --help` did a real artifact lookup
// with id="help" and exited 1 on SkipReason. The fix adds a
// --help / -h / "help" short-circuit at the top of handleWhy
// that prints the canonical help page and returns 0.

package main

import (
	"os"
	"strings"
	"testing"
)

// TestHandleWhy_HelpIsInert asserts handleWhy returns 0 for
// `-h` / `--help` / `help` and never reaches the artifact-lookup
// code path. The short-circuit must run before any DB access —
// pre-fix, the call would attempt ExplainWithHint("help", "")
// against a (possibly nil) DB and exit 1 on SkipReason.
func TestHandleWhy_HelpIsInert(t *testing.T) {
	// Redirect stdout so the printWhyHelp output doesn't pollute
	// the test runner.
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	for _, helpTok := range []string{"-h", "--help", "help"} {
		t.Run(helpTok, func(t *testing.T) {
			got := handleWhy([]string{helpTok})
			if got != 0 {
				t.Fatalf("handleWhy(%q) must short-circuit to 0; got %d. The prior regression did a real artifact lookup with id=%q.", helpTok, got, helpTok)
			}
		})
	}

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	os.Stdout = oldStdout
	buf := make([]byte, 16384)
	n, _ := r.Read(buf)
	out := string(buf[:n])
	if !strings.Contains(out, "MPM · Why") {
		t.Fatalf("combined printWhyHelp output must contain canonical heading `MPM · Why`; got:\n%s", out)
	}
}

// TestPrintWhyHelp_CanonicalHeading asserts printWhyHelp renders
// the canonical `MPM · Why` heading (not a marketing heading or a
// help-only diverging visual grammar).
func TestPrintWhyHelp_CanonicalHeading(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	oldStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = oldStdout }()

	printWhyHelp()

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	buf := make([]byte, 16384)
	n, _ := r.Read(buf)
	out := string(buf[:n])

	if !strings.Contains(out, "MPM · Why") {
		t.Fatalf("printWhyHelp output must contain canonical heading `MPM · Why`; got:\n%s", out)
	}
	// Tagline from the legacy `mpm help` must NOT bleed into the
	// why help page (the cognitive-interface principle).
	for _, phrase := range []string{"Your long-term memory", "always within reach"} {
		if strings.Contains(out, phrase) {
			t.Fatalf("printWhyHelp must not contain legacy tagline %q; got:\n%s", phrase, out)
		}
	}
	// Implementation-archaeology phrases from prior waves.
	for _, phrase := range []string{"Wave 2", "cognitive-interface RFC", "single source of truth", "Round 9", "T54b", "facade"} {
		if strings.Contains(out, phrase) {
			t.Fatalf("printWhyHelp must not contain archaeology %q; got:\n%s", phrase, out)
		}
	}
}

// TestWhyHelp_KindVocabulary pins the exact `--kind` vocabulary
// that handleWhy accepts. Update this test if the canonical set
// changes. The verified set is:
//   memory, decision, theory, skill, lesson, work
func TestWhyHelp_KindVocabulary(t *testing.T) {
	wantVocabulary := []string{"memory", "decision", "theory", "skill", "lesson", "work"}
	for _, kind := range wantVocabulary {
		t.Run("kind_"+kind, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("os.Pipe: %v", err)
			}
			oldStdout := os.Stdout
			os.Stdout = w
			defer func() { os.Stdout = oldStdout }()
			printWhyHelp()
			_ = w.Close()
			buf := make([]byte, 16384)
			n, _ := r.Read(buf)
			out := string(buf[:n])
			if !strings.Contains(out, kind) {
				t.Fatalf("printWhyHelp output must document --kind vocabulary entry %q; got:\n%s", kind, out)
			}
		})
	}
}
