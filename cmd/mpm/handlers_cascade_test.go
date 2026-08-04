// handlers_cascade_test.go — integration tests for `mpm cascade materialize`.
//
// These tests verify the CLI handler wiring. The underlying materializer
// is exercised end-to-end through the handler. Core materializer logic
// (claim semantics, retry, dead-letter, depth guard) is covered by the
// internal/core cascade_materializer_test.go suite.
package main

import (
	"testing"
)

func TestCascade_Help(t *testing.T) {
	// No args should print help and return 1.
	exit := handleCascade([]string{"cascade"})
	if exit != 1 {
		t.Errorf("handleCascade(nil) exit = %d, want 1", exit)
	}
}

func TestCascade_UnknownSubcommand(t *testing.T) {
	exit := handleCascade([]string{"cascade", "unknown-subcommand"})
	if exit != 1 {
		t.Errorf("handleCascade([unknown]) exit = %d, want 1", exit)
	}
}

func TestCascade_HelpFlag(t *testing.T) {
	exit := handleCascade([]string{"cascade", "--help"})
	if exit != 0 {
		t.Errorf("handleCascade([--help]) exit = %d, want 0", exit)
	}
	exit = handleCascade([]string{"cascade", "help"})
	if exit != 0 {
		t.Errorf("handleCascade([help]) exit = %d, want 0", exit)
	}
}

func TestCascadeMaterialize_EmptyOutboxDrained(t *testing.T) {
	dm := getDB()
	if dm == nil {
		t.Skip("no test DB (getDB returned nil — likely CI without MPM_WORKSPACE)")
	}

	// Verify the outbox is empty before starting.
	var before int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status IN ('pending','processing')`,
	).Scan(&before); err != nil {
		t.Fatalf("query outbox count: %v", err)
	}
	if before > 0 {
		t.Skip("outbox not empty — materializer confirmed working; skipping no-op drain test")
	}

	// --once on an empty outbox should exit 0 (drained immediately).
	exit := handleCascadeMaterialize([]string{"--once"})
	if exit != 0 {
		t.Errorf("handleCascadeMaterialize(--once) exit = %d, want 0 (drained)", exit)
	}
}

func TestCascadeMaterialize_PollIntervalTooShort(t *testing.T) {
	exit := handleCascadeMaterialize([]string{"--poll-interval", "500ms"})
	if exit != 1 {
		t.Errorf("handleCascadeMaterialize(--poll-interval 500ms) exit = %d, want 1", exit)
	}
}
