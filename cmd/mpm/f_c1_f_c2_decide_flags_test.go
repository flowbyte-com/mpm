// f_c1_f_c2_decide_flags_test.go — F-C1/F-C2 mpm decide flag parser
// regression coverage.
//
// F-C1: `mpm decide --context <text> --choice <text> --rationale <text>`
//       must work — previously the flag form silently produced an
//       empty decision because the parser only recognised the colon
//       tokens ("CHOICE: foo").
//
// F-C2: passing flags without --choice must be rejected, not silently
//       recorded. The pre-fix code created an empty decision memory
//       whose content was literally "CHOICE: " with no body.
package main

import (
	"strings"
	"testing"
)

// TestF_C1_DecideFlagFormProducesContent exercises the flag form
// through parseDecisionArgs. We don't call handleRecordDecision
// directly because it touches the DB; the parser is the load-bearing
// unit.
func TestF_C1_DecideFlagFormProducesContent(t *testing.T) {
	args := []string{
		"--context", "high contention",
		"--choice", "WAL journal mode",
		"--rationale", "concurrent readers, single writer",
		"--tags", "sqlite,alpha-3",
	}
	ctx, choice, rationale, tags, free, leftovers := parseDecisionArgs(args)

	if ctx != "high contention" {
		t.Errorf("context: got %q, want %q", ctx, "high contention")
	}
	if choice != "WAL journal mode" {
		t.Errorf("choice: got %q, want %q", choice, "WAL journal mode")
	}
	if rationale != "concurrent readers, single writer" {
		t.Errorf("rationale: got %q, want %q", rationale, "concurrent readers, single writer")
	}
	if tags != "sqlite,alpha-3" {
		t.Errorf("tags: got %q, want %q", tags, "sqlite,alpha-3")
	}
	if free != "" {
		t.Errorf("flag form should leave free-text empty, got %q", free)
	}
	if len(leftovers) != 0 {
		t.Errorf("flag form should consume all args, leftovers=%v", leftovers)
	}
}

// TestF_C1_ShortFlagsAccepted pins the short-flag aliases so agents
// using one-letter flags don't break.
func TestF_C1_ShortFlagsAccepted(t *testing.T) {
	args := []string{
		"-c", "ctx",
		"--choice", "ch",
		"-r", "rat",
		"-t", "t1,t2",
	}
	ctx, choice, rationale, tags, _, _ := parseDecisionArgs(args)
	if ctx != "ctx" || choice != "ch" || rationale != "rat" || tags != "t1,t2" {
		t.Fatalf("short flags: ctx=%q choice=%q rationale=%q tags=%q", ctx, choice, rationale, tags)
	}
}

// TestF_C1_LegacyTokenFormStillWorks pins backward compatibility —
// the colon-token form must continue to parse so existing call
// sites and agent_installation entries don't break.
//
// The legacy form expects a single string with embedded newlines
// (the historical call-site shape: one positional arg, colon tokens
// separated by \n). Modern agents usually pass flag form; the
// legacy form is here so we don't break older deployments.
func TestF_C1_LegacyTokenFormStillWorks(t *testing.T) {
	args := []string{
		"CHOICE: enable WAL\nCONTEXT: high contention\nRATIONALE: concurrent readers\nTAGS: sqlite,alpha-3",
	}
	ctx, choice, rationale, tags, _, _ := parseDecisionArgs(args)
	if ctx != "high contention" {
		t.Errorf("legacy context: got %q", ctx)
	}
	if choice != "enable WAL" {
		t.Errorf("legacy choice: got %q", choice)
	}
	if rationale != "concurrent readers" {
		t.Errorf("legacy rationale: got %q", rationale)
	}
	if tags != "sqlite,alpha-3" {
		t.Errorf("legacy tags: got %q", tags)
	}
}

// TestF_C1_FlagFormTakesPrecedenceWhenBothPresent pins the
// precedence contract: when both flag form and legacy tokens appear
// in args, flag form wins.
func TestF_C1_FlagFormTakesPrecedenceWhenBothPresent(t *testing.T) {
	args := []string{
		"--choice", "flag-choice",
		"CHOICE: legacy-choice",
	}
	_, choice, _, _, _, _ := parseDecisionArgs(args)
	if choice != "flag-choice" {
		t.Errorf("flag should win, got %q", choice)
	}
}

// TestF_C2_FlagFormWithoutChoiceIsRejected pins the silent-empty
// bug fix: passing --context/--rationale without --choice must be
// flagged at the gate, not silently recorded.
func TestF_C2_FlagFormWithoutChoiceIsRejected(t *testing.T) {
	args := []string{"--context", "ctx only", "--rationale", "rat only"}
	if !hasDecisionFlags(args) {
		t.Fatal("hasDecisionFlags should detect flags")
	}
	_, choice, _, _, _, _ := parseDecisionArgs(args)
	if choice != "" {
		t.Fatalf("no --choice should yield empty choice, got %q", choice)
	}
	// The handler itself must exit 1, not silently save an empty row.
	if rc := handleRecordDecision(args); rc != 1 {
		t.Errorf("handleRecordDecision(no --choice) should exit 1, got %d", rc)
	}
}

// TestF_C2_EmptyArgsIsRejected pins the no-args usage error.
func TestF_C2_EmptyArgsIsRejected(t *testing.T) {
	if rc := handleRecordDecision(nil); rc != 1 {
		t.Errorf("empty args: got %d, want 1", rc)
	}
	if rc := handleRecordDecision([]string{}); rc != 1 {
		t.Errorf("empty slice: got %d, want 1", rc)
	}
}

// TestF_C2_NonFlagFreeTextAloneIsAccepted pins the free-text form:
// the legacy "type the choice as free text" path still works for
// agent_installation entries that don't use flags or tokens.
func TestF_C2_NonFlagFreeTextAloneIsAccepted(t *testing.T) {
	args := []string{"a quick decision"}
	_, choice, _, _, _, _ := parseDecisionArgs(args)
	if choice == "" {
		t.Fatalf("free-text choice should be parsed, got empty")
	}
	if !strings.Contains(choice, "a quick decision") {
		t.Errorf("choice should contain free text, got %q", choice)
	}
}