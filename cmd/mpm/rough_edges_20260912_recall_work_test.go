// rough_edges_20260912_recall_work_test.go — regression pins (split of the 2026-09-12 rough-edge closure suite) for the 2026-09-12
// rough-edge closure pass (follow-up to the 93-check CLI acceptance run).
// Each item is classified FIX / CONTRACT / REMOVE / DEFER in the pass
// report; the tests below pin the FIX items and CONTRACT exhibits that
// live in this package. Item numbers match the pass scope list.
package main

import (
	"database/sql"
	"strings"
	"testing"
)



// Item 4 (FIX): recall must find documents whose verbatim text breaks FTS5
// MATCH syntax (e.g. `-` parsed as NOT, or multi-KB token runs), via the
// same zero-row LIKE fallback `memory search` (FullTextSearch) uses.
// Pre-fix these returned silent zero hits.
func TestRough_Item4_RecallVerbatimFallback(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	hyphenContent := "ROUGH4-HYPHEN-TOKEN some following words"
	longContent := "ROUGH4-LONG-ANCHOR " + strings.Repeat("wibble ", 3500)
	insertMemory(t, db, "rough4-hyphen", "memories", hyphenContent, "sess1", `[]`)
	insertMemory(t, db, "rough4-long", "memories", longContent, "sess1", `[]`)
	if _, err := db.Exec(`CREATE VIRTUAL TABLE memories_fts USING fts5(content)`); err != nil {
		t.Fatalf("create fts: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memories_fts(rowid, content) SELECT rowid, content FROM memories`); err != nil {
		t.Fatalf("populate fts: %v", err)
	}

	collect := func(query string) []string {
		t.Helper()
		rows, err := keywordSearchWithTime(db, query, "memories", "", "", 0, "", 10)
		if err != nil {
			t.Fatalf("keywordSearchWithTime(%q): %v", query, err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id, content string
			var sess, tags, meta, lastAcc, refID sql.NullString
			var created any
			var rc, w float64
			if err := rows.Scan(&id, &content, &sess, &tags, &meta, &created, &rc, &w, &lastAcc, &refID); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		return ids
	}

	// Control: ordinary FTS query still resolves via the FTS path.
	if got := collect("following words"); !containsStr(got, "rough4-hyphen") {
		t.Errorf("control FTS query missed hyphen doc (got %v)", got)
	}
	// Hyphen-led verbatim: FTS MATCH errors (NOT syntax) — LIKE must save it.
	if got := collect("ROUGH4-HYPHEN-TOKEN"); !containsStr(got, "rough4-hyphen") {
		t.Errorf("hyphen verbatim query missed doc (got %v)", got)
	}
	// 20 KB verbatim blob: must resolve to its document, not zero hits.
	if got := collect(longContent); !containsStr(got, "rough4-long") {
		t.Errorf("long verbatim query missed doc (got %v)", got)
	}
	// Genuinely absent term stays absent (fallback must not invent hits).
	if got := collect("rough4-no-such-term-xyz"); len(got) != 0 {
		t.Errorf("absent term should yield zero rows (got %v)", got)
	}
}

// Item 4 (FIX): the no-results echo must stay bounded for pathological
// queries instead of reprinting tens of KB.
func TestRough_Item4_BoundQueryEcho(t *testing.T) {
	if got := boundQueryEcho("short"); got != "short" {
		t.Errorf("short query echo = %q, want verbatim", got)
	}
	big := strings.Repeat("q", 30000)
	got := boundQueryEcho(big)
	if len(got) >= len(big) {
		t.Errorf("long echo not bounded (%d bytes)", len(got))
	}
	if !strings.Contains(got, "30000") {
		t.Errorf("long echo should state total bytes, got %q...", got[:80])
	}
}

// Item 9 (FIX): `--status all` is a valid LIST filter (no constraint)
// but stays rejected for create, where the substrate hardcodes open and
// a permissive gate would mask typos (Round 9 T57 rationale preserved).
func TestRough_Item9_StatusAllListOnly(t *testing.T) {
	t.Setenv("MPM_WORKSPACE", t.TempDir()) // hermetic: see grammar test note
	if code := runWorkItemArgsForTest([]string{"list", "--status", "all"}); code != 0 {
		t.Errorf("list --status all exited %d, want 0", code)
	}
	if code := runWorkItemArgsForTest([]string{"create", "rough9-title", "--status", "all"}); code == 0 {
		t.Errorf("create --status all exited 0, want rejection")
	}
	if code := runWorkItemArgsForTest([]string{"list", "--status", "bogus"}); code == 0 {
		t.Errorf("list --status bogus exited 0, want rejection")
	}
}

