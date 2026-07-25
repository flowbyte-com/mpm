// watch_skills_test.go — parity test for the (former) watch daemon's
// `kind: skill` markdown ingestion path.
//
// Background: the MPM file-watcher daemon was deprecated and removed in
// commit e1bc707 (2026-06-26). The replacement ingestion path is the
// `mpm save-skill --file <path>` CLI, which routes through the same
// `dm.SaveSkill` core call any future watch daemon would have used.
//
// This test verifies the contract the spec required for the watch path:
//   1. A markdown file with `kind: skill` in its YAML frontmatter can be
//      ingested end-to-end via the CLI handler.
//   2. The resulting row lives in `collection='skills'`.
//   3. The id follows the canonical `skill:<name>-v<version>` format.
//
// The unique skill name comes from `t.Name()` so a re-run after a partial
// failure doesn't collide on the singleton workspace DB.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestWatchIngestSkillMarkdown(t *testing.T) {
	dir := t.TempDir()
	// Unique name per invocation so we don't collide on the singleton
	// workspace DB across reruns. Validate the name component per the
	// skill-naming rules (no colons, no whitespace).
	name := fmt.Sprintf("watchtest-%s", sanitizeForSkillID(t.Name()))
	version := "2.0.0"
	skillPath := filepath.Join(dir, name+"-v"+version+".md")
	content := fmt.Sprintf(`---
name: %s
version: %s
when_to_use: agentshell, theme
kind: skill
---
# AgentShell
body`, name, version)
	if err := os.WriteFile(skillPath, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	dm := getDB()
	if dm == nil {
		t.Skip("no test DB (getDB returned nil — likely CI without MPM_WORKSPACE)")
	}
	// Don't close the singleton — other tests share it.

	// Simulate the watch path: end-to-end via the CLI handler the way
	// `mpm save-skill --file <path>` invokes. This is the same contract
	// a future watch daemon would replay (read file → dm.SaveSkill).
	//
	// Pre-clean: the singleton DB persists across runs and the test
	// name is stable, so a row from a previous run would collide on
	// (name, version). Shred any prior row first.
	id := fmt.Sprintf("skill:%s-v%s", name, version)
	if _, err := dm.SQLDB().Exec(
		`DELETE FROM memories WHERE id = ?`, id); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}

	exit := handleSaveSkill([]string{"--file", skillPath})
	if exit != 0 {
		t.Fatalf("handleSaveSkill exit = %d", exit)
	}

	// Verify the row exists with the canonical id and collection='skills'.
	var collection string
	err := dm.SQLDB().QueryRow(
		`SELECT collection FROM memories WHERE id = ?`, id).Scan(&collection)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if collection != "skills" {
		t.Errorf("collection = %q, want skills", collection)
	}
}

// sanitizeForSkillID strips characters the skill-naming rules reject
// (colons, whitespace, slashes) from a test name so it can be used
// directly as the `name` field of a skill.
func sanitizeForSkillID(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case ':', '/', ' ', '\t', '\n':
			out = append(out, '-')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
