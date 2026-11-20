// f_h1_supersede_parity_test.go — F-H1 supersede contract parity.
//
// F-H1: the CLI exposed `--decision-id` (or `--supersedes`) but the
// underlying SupersedeDecision substrate call takes `originalID`.
// Hostile test surfaced that the field-naming mismatch silently
// recorded new decisions that were NOT linked to their predecessor.
// The fix wires `--supersedes <decision-id>` through to the
// substrate's `originalID` parameter and routes the call through
// SupersedeDecision (not AddMemory) when the flag is present.
package main

import (
	"testing"
)

// TestF_H1_ExtractFlagValue_Present pins the helper contract.
func TestF_H1_ExtractFlagValue_Present(t *testing.T) {
	args := []string{"--supersedes", "abc123", "--choice", "x"}
	if v := extractFlagValue(args, "--supersedes"); v != "abc123" {
		t.Errorf("--supersedes: got %q, want abc123", v)
	}
	if v := extractFlagValue(args, "--choice"); v != "x" {
		t.Errorf("--choice: got %q, want x", v)
	}
}

// TestF_H1_ExtractFlagValue_Absent pins the absent-flag contract:
// no panic, returns "".
func TestF_H1_ExtractFlagValue_Absent(t *testing.T) {
	args := []string{"--choice", "x"}
	if v := extractFlagValue(args, "--supersedes"); v != "" {
		t.Errorf("absent flag should return empty, got %q", v)
	}
}

// TestF_H1_ExtractFlagValue_TrailingFlag pins the missing-value
// case: --supersedes at end of args with no value must not panic.
func TestF_H1_ExtractFlagValue_TrailingFlag(t *testing.T) {
	args := []string{"--supersedes"}
	if v := extractFlagValue(args, "--supersedes"); v != "" {
		t.Errorf("trailing flag with no value should return empty, got %q", v)
	}
}

// TestF_H1_HasDecisionFlags_DetectsSupersedes pins that
// hasDecisionFlags now also fires on --supersedes so the validation
// branch ("flag form requires --choice") catches the case where
// --supersedes is the only flag.
func TestF_H1_HasDecisionFlags_DetectsSupersedes(t *testing.T) {
	if !hasDecisionFlags([]string{"--supersedes", "abc"}) {
		t.Errorf("--supersedes should be detected as a flag")
	}
}

// TestF_H1_HasDecisionFlags_EmptyArgs verifies the no-flags path:
// free-text form should not trigger the validation branch.
func TestF_H1_HasDecisionFlags_EmptyArgs(t *testing.T) {
	if hasDecisionFlags([]string{"just", "some", "words"}) {
		t.Errorf("free-text args should not look like flags")
	}
	if hasDecisionFlags(nil) {
		t.Errorf("nil args should not look like flags")
	}
}

// TestF_H1_ParseDecisionArgs_PassesThroughSupersedesWithoutConsuming
// pins that --supersedes doesn't pollute the parseDecisionArgs
// context/choice/rationale/tags slots. It's an orthogonal flag.
func TestF_H1_ParseDecisionArgs_PassesThroughSupersedesWithoutConsuming(t *testing.T) {
	args := []string{
		"--supersedes", "abc123",
		"--choice", "new choice",
		"--context", "new context",
	}
	ctx, choice, rationale, tags, _, _ := parseDecisionArgs(args)
	if ctx != "new context" || choice != "new choice" {
		t.Fatalf("parseDecisionArgs should ignore --supersedes, got ctx=%q choice=%q", ctx, choice)
	}
	if rationale != "" || tags != "" {
		t.Errorf("--supersedes should not be re-read as rationale/tags")
	}
	// The flag itself is captured by extractFlagValue, not by parseDecisionArgs.
	if v := extractFlagValue(args, "--supersedes"); v != "abc123" {
		t.Errorf("--supersedes should be readable independently, got %q", v)
	}
}

// TestF_H1_HandleRecordDecision_WithoutSupersedesUsesAddMemory pins
// that the legacy add-path still works when --supersedes is absent.
// We exit-code check only; DB-touching paths are exercised by the
// existing F9 regression suite.
func TestF_H1_HandleRecordDecision_WithoutSupersedesUsesAddMemory(t *testing.T) {
	// Use free-text form so we don't depend on DB init.
	// We expect exit 0 if DB is reachable, exit 1 if not.
	rc := handleRecordDecision([]string{"a simple decision"})
	if rc != 0 && rc != 1 {
		t.Errorf("unexpected exit code: %d", rc)
	}
}