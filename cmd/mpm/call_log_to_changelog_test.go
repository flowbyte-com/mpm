package main

// CLI/MCP parity test for log_to_changelog. The MCP tool and the
// CLI handler both route through DatabaseManager.LogChangelogEntry,
// so this test proves the CLI binding works without duplicating
// the data-layer test in internal/changelog_mcp_test.go.
//
// Test isolation: we route the CLI handler through a per-test
// in-memory DM via the same openCallDM/testDMOverride mechanism
// the existing CLI tests use. This avoids touching the real
// workspace DB.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"mpm/internal"
)

func newCLILogChangelogDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("rand: %v", err)
	}
	dsn := "file:cli_logchg_" + hex.EncodeToString(suffix) + "?mode=memory&cache=shared"
	raw, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	dm := internal.NewDatabaseManagerForDB(raw)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return dm
}

func TestCallLogToChangelog_HappyPath(t *testing.T) {
	dm := newCLILogChangelogDM(t)
	setTestDMOverride(dm)

	const hash = "0123456789abcdef0123456789abcdef01234567"
	result, err := callLogToChangelog(map[string]interface{}{
		"fact":        "changelog prose for the test commit",
		"commit_hash": hash,
		"tags":        "test,smoke",
	})
	if err != nil {
		t.Fatalf("callLogToChangelog: %v", err)
	}
	if success, _ := result.(map[string]interface{})["success"].(bool); !success {
		t.Errorf("result.success not true: %+v", result)
	}
	if got, _ := result.(map[string]interface{})["commit_hash"].(string); got != hash {
		t.Errorf("commit_hash = %q, want %q", got, hash)
	}
	if got, _ := result.(map[string]interface{})["collection"].(string); got != "changelog" {
		t.Errorf("collection = %q, want %q", got, "changelog")
	}
}

func TestCallLogToChangelog_RequiresFact(t *testing.T) {
	dm := newCLILogChangelogDM(t)
	setTestDMOverride(dm)

	_, err := callLogToChangelog(map[string]interface{}{
		"commit_hash": "0123456789abcdef0123456789abcdef01234567",
	})
	if err == nil {
		t.Fatal("expected error for empty fact, got nil")
	}
	if !strings.Contains(err.Error(), "fact is required") {
		t.Errorf("error should mention fact requirement, got: %v", err)
	}
}

func TestCallLogToChangelog_RequiresCommitHash(t *testing.T) {
	dm := newCLILogChangelogDM(t)
	setTestDMOverride(dm)

	_, err := callLogToChangelog(map[string]interface{}{
		"fact": "orphan entry",
	})
	if err == nil {
		t.Fatal("expected error for empty commit_hash, got nil")
	}
	if !strings.Contains(err.Error(), "commit_hash is required") {
		t.Errorf("error should mention commit_hash requirement, got: %v", err)
	}
}

func TestCallLogToChangelog_RejectsMalformedCommit(t *testing.T) {
	dm := newCLILogChangelogDM(t)
	setTestDMOverride(dm)

	_, err := callLogToChangelog(map[string]interface{}{
		"fact":        "body",
		"commit_hash": "abc1234", // short hash
	})
	if err == nil {
		t.Fatal("expected error for malformed commit_hash, got nil")
	}
}

// TestCallLogToChangelog_ToolRegistered: prove the CLI tool
// registry has the binding. If someone removes the entry from
// the toolRegistry map, this test fails immediately.
func TestCallLogToChangelog_ToolRegistered(t *testing.T) {
	_, ok := toolRegistry["log_to_changelog"]
	if !ok {
		t.Fatal("log_to_changelog not registered in toolRegistry — CLI binding missing")
	}
}
