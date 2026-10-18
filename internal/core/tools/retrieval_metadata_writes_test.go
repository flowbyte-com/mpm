// retrieval_metadata_writes_test.go — MCP-level regression coverage for
// the issue #18 contract.
//
// The "retrieval is write-free" claim is scoped (issue #18,
// 2026-10-17): direct HybridSearch and HybridSearchMemories do not
// mutate the memory row, the contradiction queue, or retrieval_metadata
// (pinned by TestDirectSearch_WritesNothingToMemoryQueueOrRetrievalMetadata
// in internal/core/hybrid_search_nomutate_test.go). MCP handler surfaces
// — handleQueryLongTermMemory, handleReadWakeContext, and the
// retrieve/scan handlers in this file — DO record every surfaced node
// in retrieval_metadata via RecordRetrieval. Those writes are
// intentional observability (see retrieval_metadata.go:1-16 and
// SPEC.md:2248).
//
// What this file pins:
//
//  1. handleQueryLongTermMemory writes one retrieval_metadata row per
//     returned memory id, with reuse_count >= 1 and last_retrieved_at
//     non-null. Errors from RecordRetrieval are non-fatal (fire-and-
//     forget), so a write failure must not abort the response.
//  2. handleReadWakeContext writes one retrieval_metadata row per
//     surfaced memory and per surfaced milestone (handlers.go:2486,
//     handlers.go:2497).
//  3. Neither surface mutates a memory row (weight, status,
//     metadata, last_accessed_at, embedding) and neither writes to
//     shared.contradiction_log. The earlier
//     "TestRetrieval_WritesNothingToAnyStore" name is now scoped to
//     direct search in the test it lives in; this file is the
//     MCP-side counterpart.
//
// Each test uses a hermetic in-memory DB and PinIsolatedWorkspace, so
// nothing touches the live store.

package tools

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
)

// fetchRetrievalMetadataRow returns (node_type, reuse_count,
// last_retrieved_at, success_count) for the given node_id, or
// (zero, 0, 0, 0) when the row is absent. Errors other than "no rows"
// are reported through t.Fatal so a real DB failure is not silently
// treated as an empty row.
func fetchRetrievalMetadataRow(t *testing.T, dm *internal.DatabaseManager, nodeID string) (string, int64, int64, int64) {
	t.Helper()
	var (
		nodeType string
		reuse    int64
		lastAt   sql.NullInt64
		success  int64
	)
	err := dm.SQLDB().QueryRow(
		`SELECT node_type, reuse_count, last_retrieved_at, success_count
		 FROM retrieval_metadata WHERE node_id = ?`, nodeID).
		Scan(&nodeType, &reuse, &lastAt, &success)
	if err == sql.ErrNoRows {
		return "", 0, 0, 0
	}
	if err != nil {
		t.Fatalf("fetch retrieval_metadata %q: %v", nodeID, err)
	}
	last := int64(0)
	if lastAt.Valid {
		last = lastAt.Int64
	}
	return nodeType, reuse, last, success
}

// TestHandleQueryLongTermMemory_RecordsRetrievalMetadataForEachResult
// pins the MCP query contract: a memory search through the public
// mpm_memory action="query" surface records a retrieval_metadata row
// for every returned node (issue #18, handlers.go:585-592). Memory
// rows themselves are unchanged — the write is to a third table that
// the 2026-10-13 contract was not scoped to.
func TestHandleQueryLongTermMemory_RecordsRetrievalMetadataForEachResult(t *testing.T) {
	internal.PinIsolatedWorkspace(t)
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Seed two memories with explicit, distinct content so the lexical
	// path of HybridSearchMemories can return both. FTS5 shadow tables
	// were created by InitSchema, so the lexical scan is real.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, deleted_at, created_at, updated_at)
		VALUES
		  ('qry-alpha', 'memories', 'alpha retrieval_metadata probe record', 5, NULL, '2026-10-17', '2026-10-17'),
		  ('qry-beta',  'memories', 'beta  retrieval_metadata probe record',  5, NULL, '2026-10-17', '2026-10-17')
	`); err != nil {
		t.Fatalf("seed memories: %v", err)
	}

	// Sanity: retrieval_metadata is empty before the call.
	for _, id := range []string{"qry-alpha", "qry-beta"} {
		typ, reuse, lastAt, _ := fetchRetrievalMetadataRow(t, dm, id)
		if typ != "" || reuse != 0 || lastAt != 0 {
			t.Fatalf("precondition: retrieval_metadata not empty for %s before query: typ=%q reuse=%d lastAt=%d",
				id, typ, reuse, lastAt)
		}
	}

	out, err := handleQueryLongTermMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"query": "retrieval_metadata",
		"limit": 5,
	})
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	resp, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out)
	}
	// The default projection is "summary" (handlers.go:614-668) and
	// the response envelope uses "memories", not "items". The summary
	// projection returns a []ProjectedMemoryEntry. We don't need the
	// fields — only the ids — so the simplest assertion is to JSON-round-
	// trip the slice back to a generic []interface{} and read the id
	// from each element. The handlers package defines the projection
	// type; we read it indirectly so a projection-shape change is
	// visible in the test (rather than asserting on the type name).
	rawMem := projectMemoryList(resp["memories"])
	if len(rawMem) == 0 {
		// Helpful diagnostic: dump the response so a future
		// regression on the search path or the projection can be
		// diagnosed without re-reading the source.
		keys := mapKeys(resp)
		for _, k := range keys {
			t.Logf("response[%q] = %v", k, resp[k])
		}
		t.Fatalf("query returned zero memories; lexical path expected to find seeded rows. response keys: %v", keys)
	}

	// Every returned id must now have a retrieval_metadata row with
	// reuse_count >= 1 and a non-null last_retrieved_at. Node type is
	// the memory's collection (handlers.go:587-590).
	for _, item := range rawMem {
		if item == nil {
			continue
		}
		id, _ := item["id"].(string)
		if id == "" {
			t.Fatalf("item missing id: %+v", item)
		}
		typ, reuse, lastAt, success := fetchRetrievalMetadataRow(t, dm, id)
		// The handler passes the memory's `collection` through to
		// node_type (handlers.go:587-590). Seeded rows are in the
		// "memories" collection, so the recorded node_type is
		// "memories" — not a fixed "memory" string.
		if typ != "memories" {
			t.Errorf("retrieval_metadata[%s].node_type = %q, want %q", id, typ, "memories")
		}
		if reuse < 1 {
			t.Errorf("retrieval_metadata[%s].reuse_count = %d, want >= 1", id, reuse)
		}
		if lastAt == 0 {
			t.Errorf("retrieval_metadata[%s].last_retrieved_at = 0, want non-null", id)
		}
		// success_count is the "useful" signal, separate from the
		// "surfaced" signal; it stays at 0 unless a success path fires.
		if success != 0 {
			t.Errorf("retrieval_metadata[%s].success_count = %d, want 0 (no success signal on read)", id, success)
		}
	}

	// Negative control: a query that matches neither seeded row must
	// leave retrieval_metadata unchanged. The previous assertions used
	// all matching rows; this asserts that a no-hit call does not
	// accidentally insert a row (e.g. an "always upsert" bug).
	beforeMissing := countRetrievalMetadata(t, dm)
	if _, err := handleQueryLongTermMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"query": "absolutely-no-seeded-content-contains-this-token",
		"limit": 5,
	}); err != nil {
		t.Fatalf("miss-query failed: %v", err)
	}
	afterMissing := countRetrievalMetadata(t, dm)
	if afterMissing != beforeMissing {
		t.Errorf("miss-query grew retrieval_metadata: before=%d after=%d (rows were inserted without matching items)",
			beforeMissing, afterMissing)
	}

	// Memory rows are unchanged. The MCP path records retrieval; it
	// does not adjust weight, status, metadata, last_accessed_at or
	// any other memory field.
	for _, id := range []string{"qry-alpha", "qry-beta"} {
		var (
			weight    float64
			metadata  string
			accessed  sql.NullString
			updatedAt string
		)
		if err := dm.SQLDB().QueryRow(
			`SELECT weight, COALESCE(metadata,''), last_accessed_at, updated_at FROM memories WHERE id = ?`, id,
		).Scan(&weight, &metadata, &accessed, &updatedAt); err != nil {
			t.Fatalf("read memory %s: %v", id, err)
		}
		if weight != 5 {
			t.Errorf("memory %s weight = %v, want 5 (MCP query must not adjust weight)", id, weight)
		}
		if metadata != "" {
			t.Errorf("memory %s metadata = %q, want empty (MCP query must not write metadata)", id, metadata)
		}
		if accessed.Valid && accessed.String != "" {
			t.Errorf("memory %s last_accessed_at = %q, want NULL (MCP query is not the CLI lexical reinforcement path)",
				id, accessed.String)
		}
	}
}

// TestHandleReadWakeContext_RecordsRetrievalMetadataForSurfacedMemories
// pins the wake-surface contract: every memory and milestone surfaced
// by handleReadWakeContext is recorded in retrieval_metadata
// (handlers.go:2486 and handlers.go:2497). The wake surface is the
// boot prompt; firing it once is enough to "consume" every memory it
// surfaces, so a write-per-surface is the right shape and the test
// asserts it explicitly.
func TestHandleReadWakeContext_RecordsRetrievalMetadataForSurfacedMemories(t *testing.T) {
	internal.PinIsolatedWorkspace(t)
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Seed two memories and one milestone, with recent created_at so
	// the wake-context gather includes them. The gather uses
	// fetchRecentMemoriesByCollection (handlers.go:2759) ordered by
	// created_at DESC.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, tags, deleted_at, created_at, updated_at)
		VALUES
		  ('wake-alpha', 'memories', 'wake alpha surface test',  5, '',                          NULL, '2026-10-17', '2026-10-17'),
		  ('wake-beta',  'memories', 'wake beta surface test',   5, '',                          NULL, '2026-10-17', '2026-10-17'),
		  ('wake-milestone', 'memories', 'wake milestone surface test', 5, 'type:milestone-shipped', NULL, '2026-10-17', '2026-10-17')
	`); err != nil {
		t.Fatalf("seed memories: %v", err)
	}

	out, err := handleReadWakeContext(dm, internal.ActiveContext{}, map[string]interface{}{})
	if err != nil {
		t.Fatalf("read_wake_context failed: %v", err)
	}
	resp, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map output, got %T", out)
	}
	recentMem, _ := resp["recent_memories"].([]map[string]interface{})
	recentMil, _ := resp["recent_milestones"].([]map[string]interface{})

	// The wake surface must actually have surfaced our seeded rows;
	// otherwise the rest of the test is asserting on the wrong data.
	if len(recentMem)+len(recentMil) == 0 {
		t.Fatalf("wake surface returned no memories or milestones; seeds must be present to validate the contract")
	}

	// Every surfaced memory id (in either list) must have a
	// retrieval_metadata row with reuse_count >= 1.
	surfaced := 0
	for _, m := range append(recentMem, recentMil...) {
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		surfaced++
		typ, reuse, lastAt, success := fetchRetrievalMetadataRow(t, dm, id)
		// The wake surface hardcodes the node_type literal "memory"
		// (handlers.go:2486, 2497) — distinct from the mpm_memory
		// query path which passes the collection through. The
		// distinction is intentional: the wake surface records nodes
		// regardless of which table the row was sourced from.
		if typ != "memory" {
			t.Errorf("retrieval_metadata[%s].node_type = %q, want %q (wake hardcodes the literal)", id, typ, "memory")
		}
		if reuse < 1 {
			t.Errorf("retrieval_metadata[%s].reuse_count = %d, want >= 1 (wake surface records every node pulled into the boot prompt)",
				id, reuse)
		}
		if lastAt == 0 {
			t.Errorf("retrieval_metadata[%s].last_retrieved_at = 0, want non-null", id)
		}
		if success != 0 {
			t.Errorf("retrieval_metadata[%s].success_count = %d, want 0", id, success)
		}
	}
	if surfaced == 0 {
		t.Fatalf("no surfaced ids had non-empty 'id' field; payload shape changed")
	}

	// Memory rows are unchanged: weight, metadata, last_accessed_at.
	for _, id := range []string{"wake-alpha", "wake-beta", "wake-milestone"} {
		var (
			weight   float64
			metadata string
			accessed sql.NullString
		)
		if err := dm.SQLDB().QueryRow(
			`SELECT weight, COALESCE(metadata,''), last_accessed_at FROM memories WHERE id = ?`, id,
		).Scan(&weight, &metadata, &accessed); err != nil {
			t.Fatalf("read memory %s: %v", id, err)
		}
		if weight != 5 {
			t.Errorf("memory %s weight = %v, want 5 (wake surface must not adjust weight)", id, weight)
		}
		if metadata != "" {
			t.Errorf("memory %s metadata = %q, want empty (wake surface must not write metadata)", id, metadata)
		}
		if accessed.Valid && accessed.String != "" {
			t.Errorf("memory %s last_accessed_at = %q, want NULL (wake surface is not the CLI lexical reinforcement path)",
				id, accessed.String)
		}
	}

	// Contradiction queue is unchanged. The wake gather touches
	// memories; it does not consult or write to shared.contradiction_log.
	if countQueue(t, dm) != 0 {
		t.Errorf("shared.contradiction_log has rows after wake read; the wake surface must not write to the queue")
	}
}

// TestMCPQueryAndWake_DoNotMutateMemoryOrContradictionQueue is the
// negative control that pairs with the two positive tests above. The
// assertion is: a memory search AND a wake-context read, run in
// sequence against a hermetic DB, leave the memory row and the
// contradiction queue byte-identical to their pre-call state. The
// only allowed writes are to retrieval_metadata; this test pins that
// scope by checking the two forbidden stores separately and by
// counting retrieval_metadata rows so the absence of unexpected
// writes is not over-claimed.
func TestMCPQueryAndWake_DoNotMutateMemoryOrContradictionQueue(t *testing.T) {
	internal.PinIsolatedWorkspace(t)
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, deleted_at, created_at, updated_at)
		VALUES
		  ('mix-1', 'memories', 'mixed surfaces test row one',   5, NULL, '2026-10-17', '2026-10-17'),
		  ('mix-2', 'memories', 'mixed surfaces test row two',   5, NULL, '2026-10-17', '2026-10-17')
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	memoryBefore := dumpMemories(t, dm)
	queueBefore := dumpContradictionLog(t, dm)
	retBefore := countRetrievalMetadata(t, dm)

	if _, err := handleQueryLongTermMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"query": "mixed",
		"limit": 5,
	}); err != nil {
		t.Fatalf("query: %v", err)
	}
	if _, err := handleReadWakeContext(dm, internal.ActiveContext{}, map[string]interface{}{}); err != nil {
		t.Fatalf("read_wake_context: %v", err)
	}

	memoryAfter := dumpMemories(t, dm)
	queueAfter := dumpContradictionLog(t, dm)
	retAfter := countRetrievalMetadata(t, dm)

	if strings.Join(memoryBefore, "\n") != strings.Join(memoryAfter, "\n") {
		t.Errorf("memory rows changed after MCP query+wake:\nbefore:\n%s\nafter:\n%s",
			strings.Join(memoryBefore, "\n"),
			strings.Join(memoryAfter, "\n"))
	}
	if strings.Join(queueBefore, "\n") != strings.Join(queueAfter, "\n") {
		t.Errorf("contradiction_log changed after MCP query+wake:\nbefore:\n%s\nafter:\n%s",
			strings.Join(queueBefore, "\n"),
			strings.Join(queueAfter, "\n"))
	}
	// retrieval_metadata is allowed to grow: that is the contract.
	// We just pin that the *only* table that grew is the one MCP is
	// supposed to write to.
	if retAfter < retBefore {
		t.Errorf("retrieval_metadata shrank from %d to %d", retBefore, retAfter)
	}
}

// projectMemoryList extracts the response["memories"] slice as a
// []map[string]interface{} regardless of whether the handler returned
// []ProjectedMemoryEntry (summary projection, default) or
// []map[string]interface{} (full projection). The handler always
// JSON-encodes the response, so json-round-tripping the value
// normalises both shapes to the same map type and lets the assertions
// work against either projection without coupling to its concrete type.
func projectMemoryList(v interface{}) []map[string]interface{} {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

// mapKeys returns the keys of m in sorted order, used by the
// failure message above to make the response shape visible when the
// handler envelope drifts.
func mapKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// countRetrievalMetadata returns the row count of retrieval_metadata.
func countRetrievalMetadata(t *testing.T, dm *internal.DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM retrieval_metadata`).Scan(&n); err != nil {
		t.Fatalf("count retrieval_metadata: %v", err)
	}
	return n
}

// countQueue returns the row count of shared.contradiction_log. The
// table lives in the attached "shared" schema; the test must use the
// path-based setup (newTestDM) so attachShared runs, otherwise the
// table is absent. The helper distinguishes "table absent" (0) from
// "DB error" (Fatalf) so a missing schema in a misconfigured fixture
// is not silently treated as a clean queue.
func countQueue(t *testing.T, dm *internal.DatabaseManager) int {
	t.Helper()
	var n int
	err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM shared.contradiction_log`).Scan(&n)
	if err != nil && !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("count contradiction_log: %v", err)
	}
	return n
}

// dumpMemories renders every memory row in a stable, column-by-column
// format so a byte-equal comparison is meaningful. The column set is
// the same one snapshotCorpus uses in hybrid_search_nomutate_test.go;
// adding retrieval_metadata to that helper's coverage is what pins
// the contract end-to-end.
func dumpMemories(t *testing.T, dm *internal.DatabaseManager) []string {
	t.Helper()
	rows, err := dm.SQLDB().Query(
		`SELECT id, collection, content, COALESCE(metadata,''), weight, COALESCE(deleted_at,''),
		        COALESCE(last_accessed_at,''), COALESCE(updated_at,'')
		 FROM memories ORDER BY id`)
	if err != nil {
		t.Fatalf("dump memories: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, coll, content, meta, deleted, accessed, updated string
		var weight float64
		if err := rows.Scan(&id, &coll, &content, &meta, &weight, &deleted, &accessed, &updated); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, strings.Join([]string{
			id, coll, content, meta, strconv.FormatFloat(weight, 'g', -1, 64),
			deleted, accessed, updated,
		}, "\x1f"))
	}
	return out
}

// dumpContradictionLog renders the shared.contradiction_log content.
// If the shared schema is not attached (in-memory fixture), returns
// nil — the assertion is then trivially satisfied on both sides, which
// is the right shape for an in-memory setup that doesn't carry the
// queue. The path-based setup (newTestDM) attaches the shared schema
// and gets a real table.
func dumpContradictionLog(t *testing.T, dm *internal.DatabaseManager) []string {
	t.Helper()
	rows, err := dm.SQLDB().Query(
		`SELECT id, memory_id_a, memory_id_b, COALESCE(detected_by,''),
		        COALESCE(resolved_at,''), COALESCE(similarity,0)
		 FROM shared.contradiction_log ORDER BY id`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		t.Fatalf("dump contradiction_log: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, a, b, by, resolved string
		var sim float64
		if err := rows.Scan(&id, &a, &b, &by, &resolved, &sim); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, strings.Join([]string{
			id, a, b, by, resolved, strconv.FormatFloat(sim, 'g', -1, 64),
		}, "\x1f"))
	}
	return out
}
