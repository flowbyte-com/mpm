// f0910_topic_add_regression_test.go — 2026-09-10 fix.
//
// Bug: `mpm topic add --name X` created a topic literally named
// "--name" because the parser blindly assigned `args[0]` as the
// topic name without recognising the flag. The fix introduces a
// dedicated `parseTopicAddArgs` helper that recognizes `--name`,
// `--description`, and the positional form, and fails loudly on
// unknown flags.
package main

import (
	"strings"
	"testing"
)

// TestTopicAdd_PositionalForm pins the legacy positional form:
// `mpm topic add <name> [description]` must continue to work.
func TestTopicAdd_PositionalForm(t *testing.T) {
	name, desc, err := parseTopicAddArgs([]string{"alpha-positional", "alpha description"})
	if err != nil {
		t.Fatalf("positional form: %v", err)
	}
	if name != "alpha-positional" {
		t.Errorf("name: got %q, want alpha-positional", name)
	}
	if desc != "alpha description" {
		t.Errorf("description: got %q, want 'alpha description'", desc)
	}
}

// TestTopicAdd_NameFlag pins requirement: `mpm topic add --name X`
// must use X as the topic name (NOT "--name").
func TestTopicAdd_NameFlag(t *testing.T) {
	name, desc, err := parseTopicAddArgs([]string{"--name", "real-name"})
	if err != nil {
		t.Fatalf("--name form: %v", err)
	}
	if name != "real-name" {
		t.Errorf("name: got %q, want real-name (the bug stored '--name' literally)", name)
	}
	if desc != "" {
		t.Errorf("description must be empty, got %q", desc)
	}
}

// TestTopicAdd_NameAndDescriptionFlags pins the full flag form.
func TestTopicAdd_NameAndDescriptionFlags(t *testing.T) {
	name, desc, err := parseTopicAddArgs([]string{
		"--name", "alpha-full",
		"--description", "alpha description text",
	})
	if err != nil {
		t.Fatalf("full flag form: %v", err)
	}
	if name != "alpha-full" {
		t.Errorf("name: got %q, want alpha-full", name)
	}
	if desc != "alpha description text" {
		t.Errorf("description: got %q", desc)
	}
}

// TestTopicAdd_QuotedNameWithSpaces pins that quoted names
// (which arrive as a single arg after shell quoting) survive.
func TestTopicAdd_QuotedNameWithSpaces(t *testing.T) {
	name, _, err := parseTopicAddArgs([]string{"--name", "name with spaces"})
	if err != nil {
		t.Fatalf("quoted: %v", err)
	}
	if name != "name with spaces" {
		t.Errorf("name: got %q, want 'name with spaces'", name)
	}
}

// TestTopicAdd_MissingNameValue pins that `--name` with no value
// fails loudly rather than silently using the empty string.
func TestTopicAdd_MissingNameValue(t *testing.T) {
	_, _, err := parseTopicAddArgs([]string{"--name"})
	if err == nil {
		t.Fatal("--name without value must error")
	}
	if !strings.Contains(err.Error(), "--name") {
		t.Errorf("error must name --name, got: %v", err)
	}
}

// TestTopicAdd_MissingDescriptionValue pins the same for description.
func TestTopicAdd_MissingDescriptionValue(t *testing.T) {
	_, _, err := parseTopicAddArgs([]string{"--name", "x", "--description"})
	if err == nil {
		t.Fatal("--description without value must error")
	}
	if !strings.Contains(err.Error(), "--description") {
		t.Errorf("error must name --description, got: %v", err)
	}
}

// TestTopicAdd_UnknownFlag pins that an unknown flag fails loudly
// with the offending flag name surfaced (so a typo like `--naem`
// doesn't silently drop the topic into a partial state).
func TestTopicAdd_UnknownFlag(t *testing.T) {
	_, _, err := parseTopicAddArgs([]string{"--bogus", "x"})
	if err == nil {
		t.Fatal("unknown flag must error")
	}
	if !strings.Contains(err.Error(), "--bogus") {
		t.Errorf("error must name the offending flag, got: %v", err)
	}
}

// TestTopicAdd_MixedForm pins that the flag name and positional
// description can be combined (the user's natural form when they
// type `--name X` followed by a description string).
func TestTopicAdd_MixedForm(t *testing.T) {
	name, desc, err := parseTopicAddArgs([]string{"--name", "alpha-mixed", "pos-desc"})
	if err != nil {
		t.Fatalf("mixed: %v", err)
	}
	if name != "alpha-mixed" {
		t.Errorf("name: got %q, want alpha-mixed", name)
	}
	if desc != "pos-desc" {
		t.Errorf("description: got %q, want pos-desc", desc)
	}
}

// TestTopicAdd_EmptyArgs pins that calling with no args returns
// an empty name (so the caller can decide the user-facing error).
func TestTopicAdd_EmptyArgs(t *testing.T) {
	name, _, err := parseTopicAddArgs(nil)
	if err != nil {
		t.Fatalf("nil args must not error at the parser level: %v", err)
	}
	if name != "" {
		t.Errorf("name must be empty, got %q", name)
	}
}
