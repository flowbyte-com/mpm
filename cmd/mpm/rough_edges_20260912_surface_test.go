// rough_edges_20260912_surface_test.go — regression pins (split of the 2026-09-12 rough-edge closure suite) for the 2026-09-12
// rough-edge closure pass (follow-up to the 93-check CLI acceptance run).
// Each item is classified FIX / CONTRACT / REMOVE / DEFER in the pass
// report; the tests below pin the FIX items and CONTRACT exhibits that
// live in this package. Item numbers match the pass scope list.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)




// Item 6 (FIX): `mpm ops confidence --help` (and subcommand --help) must
// print help, not "unknown subcommand" / "--artifact is required".
func TestRough_Item6_OpsConfidenceHelp(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{"help"},
		{"show", "--help"},
	} {
		out := captureBoth(t, func() {
			if code := handleOpsConfidence(args); code != 0 {
				t.Fatalf("ops confidence %v exited %d, want 0", args, code)
			}
		})
		if !strings.Contains(out, "mpm ops confidence") || !strings.Contains(out, "--artifact") {
			t.Errorf("ops confidence %v: expected usage text, got:\n%s", args, out)
		}
	}
}

// Item 7 (FIX): reference search must surface BOTH identities (chunk +
// parent doc), the JSON doc_id must be the document (pre-fix it carried
// the chunk id), and `reference show` must resolve a chunk ID pasted
// from search output. The test literally takes the search-surfaced ID
// and performs the documented follow-up.
func TestRough_Item7_ReferenceIDChain(t *testing.T) {
	dm := setupMemoryAddTest(t) // hermetic DM + singleton swap (getDB too)
	refID := fC4UniqueID(t, "rough7")
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO reference_docs (id, title, file_path, source_type, tags, total_chunks, last_indexed, import_reason, created_at)
		 VALUES (?, 'Rough7 Doc', '/tmp/rough7', 'text', '[]', 1, CAST(strftime('%s','now') AS INTEGER), 'test', CAST(strftime('%s','now') AS INTEGER))`,
		refID); err != nil {
		t.Fatalf("seed doc: %v", err)
	}
	chunkID := fC4UniqueID(t, "rough7-chunk")
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO reference_chunks (id, doc_id, chunk_index, section, content) VALUES (?, ?, 0, '', ?)`,
		chunkID, refID, "rough7 marker body gamma"); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}

	// 1. Search JSON: doc_id must be the DOCUMENT, chunk_id the chunk.
	out := captureBoth(t, func() {
		if code := handleRefSearch([]string{"search", "rough7 marker", "--json"}); code != 0 {
			t.Fatalf("search exited %d", code)
		}
	})
	var senv struct {
		Results []map[string]interface{} `json:"results"`
	}
	if err := json.Unmarshal([]byte(lastJSONObject(t, out)), &senv); err != nil {
		t.Fatalf("search --json: %v\n%s", err, out)
	}
	if len(senv.Results) == 0 {
		t.Fatalf("search found nothing:\n%s", out)
	}
	if senv.Results[0]["doc_id"] != refID {
		t.Errorf("doc_id = %v, want document %s", senv.Results[0]["doc_id"], refID)
	}
	if senv.Results[0]["chunk_id"] != chunkID {
		t.Errorf("chunk_id = %v, want chunk %s", senv.Results[0]["chunk_id"], chunkID)
	}

	// 2. Search human output names both IDs.
	human := captureBoth(t, func() {
		if code := handleRefSearch([]string{"search", "rough7 marker"}); code != 0 {
			t.Fatalf("search exited %d", code)
		}
	})
	if !strings.Contains(human, refID[:8]) || !strings.Contains(human, chunkID[:8]) {
		t.Errorf("human search should name both chunk and doc IDs:\n%s", human)
	}

	// 3. The chunk ID from search resolves via show (document path).
	shown := captureBoth(t, func() {
		if code := handleRefShow([]string{"show", chunkID}); code != 0 {
			t.Fatalf("show <chunk-id> exited %d", code)
		}
	})
	if !strings.Contains(shown, refID) {
		t.Errorf("show <chunk-id> should resolve to doc %s:\n%s", refID, shown)
	}
	// 4. The doc ID still works directly.
	captureBoth(t, func() {
		if code := handleRefShow([]string{"show", refID}); code != 0 {
			t.Fatalf("show <doc-id> exited %d", code)
		}
	})
}
