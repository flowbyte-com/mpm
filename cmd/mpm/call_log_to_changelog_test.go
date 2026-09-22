package main

// CLI/MCP parity test for mpm_log_to_changelog. The MCP tool and the
// CLI handler both route through DatabaseManager.LogChangelogEntry,
// so this test proves the CLI binding works without duplicating
// the data-layer test in internal/changelog_mcp_test.go.
//
// Test isolation: we route the CLI handler through a per-test
// in-memory DM via the same openCallDM/testDMOverride mechanism
// the existing CLI tests use. This avoids touching the real
// workspace DB.

import (
	"strings"
	"testing"


	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
)

func newCLILogChangelogDM(t *testing.T) *internal.DatabaseManager {
	t.Helper()
	return internal.NewTestDM(t)
}

func TestCallLogToChangelog_HappyPath(t *testing.T) {
	dm := newCLILogChangelogDM(t)

	const hash = "0123456789abcdef0123456789abcdef01234567"
	result, err := runHandler(dm, "mpm_log_to_changelog", map[string]interface{}{
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

	_, err := runHandler(dm, "mpm_log_to_changelog", map[string]interface{}{
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

	_, err := runHandler(dm, "mpm_log_to_changelog", map[string]interface{}{
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

	_, err := runHandler(dm, "mpm_log_to_changelog", map[string]interface{}{
		"fact":        "body",
		"commit_hash": "abc1234", // short hash
	})
	if err == nil {
		t.Fatal("expected error for malformed commit_hash, got nil")
	}
}

// TestCallLogToChangelog_ToolRegistered: prove the CLI tool
// registry has the binding. If someone removes the entry from
// the tools.ByName map, this test fails immediately.
func TestCallLogToChangelog_ToolRegistered(t *testing.T) {
	_, ok := tools.ByName("mpm_log_to_changelog")
	if !ok {
		t.Fatal("mpm_log_to_changelog not registered in tools.ByName — CLI binding missing")
	}
}
