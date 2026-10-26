// ops_confidence_decay_test.go — pin the operator-facing surface of the
// bounded confidence-decay pass (issue #92).
package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestOpsConfidenceDecay_DryRunThenRealPass pins the CLI contract:
// dry-run reports the batch without writing, the real pass decays the
// artifact, and an immediate second run has no work.
func TestOpsConfidenceDecay_DryRunThenRealPass(t *testing.T) {
	workspace(t)
	dm := getDB()
	if dm == nil {
		t.Fatal("getDB returned nil")
	}
	created := time.Now().Add(-200 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, weight, confidence, created_at, updated_at)
		 VALUES ('mem-cli-decay', 'memories', 'stale claim', 5, 0.8, ?, ?)`, created, created,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	code, out := runCLI(t, "ops", "confidence", "decay", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run exited %d: %q", code, out)
	}
	var dry map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &dry); err != nil {
		t.Fatalf("dry-run output is not JSON (%q): %v", out, err)
	}
	if dry["dry_run"] != true || dry["due"].(float64) != 1 || dry["changed"].(float64) != 1 {
		t.Fatalf("dry-run envelope = %v", dry)
	}
	var watermark *int64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence_checked_at FROM memories WHERE id = 'mem-cli-decay'`,
	).Scan(&watermark); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if watermark != nil {
		t.Fatalf("dry run wrote a watermark: %v", *watermark)
	}

	code, out = runCLI(t, "ops", "confidence", "decay")
	if code != 0 {
		t.Fatalf("real decay exited %d: %q", code, out)
	}
	var real map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &real); err != nil {
		t.Fatalf("decay output is not JSON (%q): %v", out, err)
	}
	if real["changed"].(float64) != 1 || real["failed"].(float64) != 0 {
		t.Fatalf("decay envelope = %v", real)
	}
	var conf float64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence FROM memories WHERE id = 'mem-cli-decay'`,
	).Scan(&conf); err != nil {
		t.Fatalf("read confidence: %v", err)
	}
	if conf >= 0.8 {
		t.Errorf("stale artifact did not decay: %v", conf)
	}

	code, out = runCLI(t, "ops", "confidence", "decay")
	if code != 0 {
		t.Fatalf("second decay exited %d: %q", code, out)
	}
	var second map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &second); err != nil {
		t.Fatalf("second output is not JSON: %v", err)
	}
	if second["due"].(float64) != 0 || second["scanned"].(float64) != 0 {
		t.Errorf("second pass did work: %v", second)
	}
}

// TestOpsConfidenceDecay_HelpMentionsDecay keeps the command discoverable.
func TestOpsConfidenceDecay_HelpMentionsDecay(t *testing.T) {
	code, out := runCLI(t, "ops", "confidence", "--help")
	if code != 0 {
		t.Fatalf("help exited %d", code)
	}
	if !strings.Contains(out, "decay") {
		t.Errorf("help does not mention the decay subcommand: %q", out)
	}
}
