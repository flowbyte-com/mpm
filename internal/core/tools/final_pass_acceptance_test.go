// final_pass_acceptance_test.go — exact acceptance repros for the
// final-pass release gate.
//
// These tests pin the real defect shapes (not adjacent regression
// suites). Each top-level section corresponds to one entry in the
// MPM Final Last-Mile Consistency Pass brief:
//
//   A — Working-context/scratchpad promotion atomicity
//   C — Large-content retrieval (>10KB memory) — bounded by default,
//       explicit full path returns complete content
//   G — `mpm why` surfaces legacy-shaped evidence + confidence history
//       rows whose stored artifact_type='memory' but whose public kind
//       is theory/decision
//
// Do not delete or weaken any of these tests without re-running the
// matching brief repro.

package tools

import (
	"fmt"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// confidenceHistoryTrigger is a canonical CHECK-valid trigger value for
// confidence_history rows seeded directly. The substrate restricts the
// column to a small enum (evidence_added / evidence_updated / etc.);
// using an arbitrary value triggers a CHECK constraint failure rather
// than silently inserting the row.
const confidenceHistoryTrigger = "manual_recompute"

// ---------------------------------------------------------------------------
// A — Scratchpad promotion atomicity
// ---------------------------------------------------------------------------
//
// The defect: promotion must be transactional. A success response must
// correspond to exactly one committed destination memory AND a consumed
// source scratchpad. A failure response must NOT leave a "ghost memory"
// or a half-consumed scratchpad that returns success-then-error semantics.
//
// Tests below exercise the four scenarios from the brief:
//   1. valid promotion
//   2. no-context promotion
//   3. induced mid-operation failure (scanner rejection inside the tx)
//   4. retry after success

func TestAcceptance_A_ScratchpadPromote_HappyPath(t *testing.T) {
	dm := newTestIsolatedDM(t)
	const sessionID = "acc-A-happy"

	// 1. flush a scratchpad
	flushResp, err := handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "flush",
		"params": map[string]interface{}{
			"session_id": sessionID,
			"thesis":     "acceptance A: promote success",
			"supporting": "context for the acceptance repro",
		},
	})
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if flushResp.(map[string]string)["status"] != "flushed" {
		t.Fatalf("flush did not return status=flushed: %+v", flushResp)
	}

	// 2. promote it
	promResp, err := handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "promote",
		"params": map[string]interface{}{
			"session_id": sessionID,
		},
	})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	prom, ok := promResp.(map[string]string)
	if !ok {
		t.Fatalf("promote response not map[string]string: %T", promResp)
	}
	if prom["status"] != "promoted" {
		t.Fatalf("promote did not return status=promoted: %+v", prom)
	}
	if prom["memory_id"] == "" {
		t.Fatalf("promote returned empty memory_id: %+v", prom)
	}

	// 3. assert exactly one destination memory exists
	memID := prom["memory_id"]
	got, err := dm.GetMemory(memID)
	if err != nil {
		t.Fatalf("destination memory not retrievable: %v", err)
	}
	if got["content"] == nil || got["content"] == "" {
		t.Fatalf("destination memory has no content: %+v", got)
	}
	tagsRaw, _ := got["tags"].(string)
	if !strings.Contains(tagsRaw, "from-scratchpad:"+sessionID) {
		t.Fatalf("destination memory missing lineage tag from-scratchpad:%s; tags=%q", sessionID, tagsRaw)
	}

	// 4. assert source scratchpad is consumed (no remaining row)
	_, err = handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read",
		"params": map[string]interface{}{
			"session_id": sessionID,
		},
	})
	if err == nil {
		t.Fatalf("source scratchpad still present after promote — atomicity violated")
	}
	if !strings.Contains(err.Error(), "no scratchpad found") {
		t.Fatalf("expected 'no scratchpad found' after consume; got %v", err)
	}
}

func TestAcceptance_A_ScratchpadPromote_NoContext(t *testing.T) {
	dm := newTestIsolatedDM(t)

	// empty session_id → must fail loudly, must NOT create a memory
	_, err := handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "promote",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("expected error on missing session_id")
	}

	// verify no memory was created with the lineage tag
	rows, err := dm.SQLDB().Query(
		`SELECT id FROM memories WHERE tags LIKE '%from-scratchpad:%' AND deleted_at IS NULL`,
	)
	if err != nil {
		t.Fatalf("query memories: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if count != 0 {
		t.Fatalf("no-context promotion leaked %d destination memory rows", count)
	}
}

func TestAcceptance_A_ScratchpadPromote_InducedFailureNoGhostState(t *testing.T) {
	dm := newTestIsolatedDM(t)
	const sessionID = "acc-A-induced-fail"

	// Flush a scratchpad whose `supporting` field carries a poison
	// phrase. The flush itself is accepted (the scanner does not run
	// on the scratchpad surface — only on the memories INSERT). The
	// scanner runs INSIDE the promote tx when the destination memory
	// is being created, so this gives us a deterministic mid-tx
	// failure: the INSERT step returns an error, the tx aborts, and
	// the scratchpad must remain intact.
	const poison = "sk-ant-this-is-a-deliberately-poisoned-payload-0123456789abcdef0123"
	_, err := handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "flush",
		"params": map[string]interface{}{
			"session_id": sessionID,
			"thesis":     "benign thesis",
			"supporting": poison,
		},
	})
	if err != nil {
		t.Fatalf("flush with poison supporting: %v", err)
	}

	// promote — must fail
	_, err = handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "promote",
		"params": map[string]interface{}{
			"session_id": sessionID,
		},
	})
	if err == nil {
		t.Fatal("expected scanner rejection on promote with poisoned supporting")
	}

	// verify no ghost memory with this lineage tag
	rows, err := dm.SQLDB().Query(
		`SELECT id FROM memories WHERE tags LIKE ? AND deleted_at IS NULL`,
		fmt.Sprintf("%%from-scratchpad:%s%%", sessionID),
	)
	if err != nil {
		t.Fatalf("query memories: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if count != 0 {
		t.Fatalf("induced failure left %d ghost memory rows", count)
	}

	// verify scratchpad is STILL present (tx rolled back)
	readResp, err := handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "read",
		"params": map[string]interface{}{
			"session_id": sessionID,
		},
	})
	if err != nil {
		t.Fatalf("scratchpad must remain after induced failure; got err=%v", err)
	}
	if readResp.(map[string]string)["thesis"] != "benign thesis" {
		t.Fatalf("scratchpad thesis not preserved after induced failure: %+v", readResp)
	}
}

func TestAcceptance_A_ScratchpadPromote_RetryAfterSuccessIsIdempotent(t *testing.T) {
	dm := newTestIsolatedDM(t)
	const sessionID = "acc-A-retry"

	_, err := handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "flush",
		"params": map[string]interface{}{
			"session_id": sessionID,
			"thesis":     "retry idempotency",
			"supporting": "first write",
		},
	})
	if err != nil {
		t.Fatalf("flush: %v", err)
	}

	first, err := handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "promote",
		"params": map[string]interface{}{
			"session_id": sessionID,
		},
	})
	if err != nil {
		t.Fatalf("first promote: %v", err)
	}
	firstID := first.(map[string]string)["memory_id"]
	if firstID == "" {
		t.Fatal("first promote returned empty memory_id")
	}

	// retry: scratchpad is already gone, so promote must report
	// "no scratchpad found" — this is the truthful response. It must
	// NOT silently create a second memory.
	second, err := handleMpmScratchpad(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "promote",
		"params": map[string]interface{}{
			"session_id": sessionID,
		},
	})
	if err == nil {
		t.Fatalf("retry must error (no scratchpad); got success=%+v", second)
	}
	if !strings.Contains(err.Error(), "no scratchpad found") {
		t.Fatalf("retry must report 'no scratchpad found'; got %v", err)
	}

	// count memories with the lineage tag — must remain exactly 1
	rows, err := dm.SQLDB().Query(
		`SELECT id FROM memories WHERE tags LIKE ? AND deleted_at IS NULL`,
		fmt.Sprintf("%%from-scratchpad:%s%%", sessionID),
	)
	if err != nil {
		t.Fatalf("query memories: %v", err)
	}
	defer rows.Close()
	count := 0
	var dupID string
	for rows.Next() {
		count++
		if err := rows.Scan(&dupID); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	if count != 1 {
		t.Fatalf("retry produced %d destination memories; expected exactly 1 (id=%s)", count, dupID)
	}
	if dupID != firstID {
		t.Fatalf("retry created a different memory id: first=%s dup=%s", firstID, dupID)
	}
}

// ---------------------------------------------------------------------------
// C — Large-content retrieval (>10KB memory)
// ---------------------------------------------------------------------------
//
// The defect: agent-facing responses for memory retrieval must remain
// bounded by default so an oversized payload cannot be dumped into a
// model's context window. The explicit full-pointer path must return
// the complete content unchanged.
//
// Memory storage is never truncated — only the WIRE representation is
// bounded, explicitly flagged, and paired with a pointer for full
// retrieval. The brief's repro verifies the contract end-to-end via
// the MCP `mpm_resolve` tool surface.

func TestAcceptance_C_LargeMemory_BoundedByDefault_FullOnDemand(t *testing.T) {
	dm := newTestIsolatedDM(t)

	const (
		startMarker = "START-MARKER-XYZ"
		tailMarker  = "TAIL-MARKER-XYZ"
		fillerSize  = 11000 // >10KB unique-content payload
	)

	// Build >10KB content with unique markers at start and tail
	filler := strings.Repeat("a", fillerSize)
	bigContent := fmt.Sprintf("%s\n%s\n%s\n", startMarker, filler, tailMarker)
	if len(bigContent) <= 10*1024 {
		t.Fatalf("test setup error: content too small (%d bytes)", len(bigContent))
	}

	// Save via the canonical MCP tool surface. mpm_memory save takes
	// `fact` as the content field (per handleSaveToMemory contract).
	saveResp, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "save",
		"params": map[string]interface{}{
			"fact": bigContent,
		},
	})
	if err != nil {
		t.Fatalf("save large memory: %v", err)
	}
	save, ok := saveResp.(map[string]interface{})
	if !ok {
		t.Fatalf("save response not map[string]interface{}: %T", saveResp)
	}
	memID, _ := save["id"].(string)
	if memID == "" {
		t.Fatalf("save response missing id: %+v", save)
	}

	// A. save response bounded to DefaultMaxInlineContentBytes (2048);
	//    the full pointer is exposed for retrieval.
	if c, ok := save["content"].(string); ok && len(c) > mpminternal.DefaultMaxInlineContentBytes {
		t.Fatalf("save response not bounded: %d bytes inline (>%d)", len(c), mpminternal.DefaultMaxInlineContentBytes)
	}
	if ptr, _ := save["pointer"].(string); ptr == "" {
		t.Fatalf("save response missing pointer for full retrieval: %+v", save)
	}

	// B. normal memory query is bounded. Default projection is `summary`,
	//    which truncates inline content to ~256 chars. Full content
	//    is reachable only via mpm_resolve {pointer}.
	queryResp, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "query",
		"params": map[string]interface{}{
			"query": startMarker,
			"limit": 5,
		},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	qr, _ := queryResp.(map[string]interface{})
	// `memories` is []ProjectedMemoryEntry in summary mode (256-char bound)
	hitsRaw, ok := qr["memories"].([]ProjectedMemoryEntry)
	if !ok {
		// fall back to a generic slice for tests using full projection
		if genericHits, ok2 := qr["memories"].([]map[string]interface{}); ok2 {
			hitsRaw = nil
			if len(genericHits) == 0 {
				t.Fatalf("query did not return any memory hits for start marker")
			}
			for _, h := range genericHits {
				if id, _ := h["id"].(string); id == memID {
					if c, ok := h["content"].(string); ok && strings.Contains(c, tailMarker) {
						t.Fatalf("normal query leaked full payload (tail marker present in %d-byte inline)", len(c))
					}
				}
			}
		} else {
			t.Fatalf("query returned unexpected memories shape: %T", qr["memories"])
		}
	}
	// In summary mode every hit's Summary is bounded to <= 256 chars
	// and the tail marker must NOT appear inline.
	for _, h := range hitsRaw {
		if h.ID == memID {
			if strings.Contains(h.Summary, tailMarker) {
				t.Fatalf("normal query leaked tail marker in %d-byte summary", len(h.Summary))
			}
			if h.Pointer == "" {
				t.Fatalf("normal query hit missing pointer for full retrieval: %+v", h)
			}
		}
	}

	// C. ordinary mpm_resolve is bounded (no full flag)
	resolveResp, err := handleMpmResolve(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"uri": fmt.Sprintf("mpm://memory/%s", memID),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rr, _ := resolveResp.(map[string]interface{})
	if c, ok := rr["content"].(string); ok {
		if len(c) > mpminternal.DefaultMaxInlineContentBytes {
			t.Fatalf("ordinary resolve not bounded: %d bytes inline (>%d)", len(c), mpminternal.DefaultMaxInlineContentBytes)
		}
		if strings.Contains(c, tailMarker) {
			t.Fatalf("ordinary resolve leaked tail marker — bounded projection broken")
		}
		if bounded, _ := rr["bounded"].(bool); !bounded {
			t.Fatalf("ordinary resolve did not surface bounded=true for large content: %+v", rr)
		}
	}

	// D. explicit full=true returns COMPLETE content (both markers)
	fullResp, err := handleMpmResolve(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"uri":  fmt.Sprintf("mpm://memory/%s", memID),
		"full": true,
	})
	if err != nil {
		t.Fatalf("resolve full: %v", err)
	}
	fr, _ := fullResp.(map[string]interface{})
	fullContent, _ := fr["content"].(string)
	if !strings.HasPrefix(fullContent, startMarker) && !strings.Contains(fullContent, startMarker) {
		t.Fatalf("full resolve missing START-MARKER; first 100 chars: %q", truncate(fullContent, 100))
	}
	if !strings.Contains(fullContent, tailMarker) {
		t.Fatalf("full resolve missing TAIL-MARKER; last 100 chars: %q", truncateTail(fullContent, 100))
	}
	if len(fullContent) != len(bigContent) {
		t.Fatalf("full resolve returned %d bytes; expected %d (complete content)", len(fullContent), len(bigContent))
	}

	// E. shred owner cleans up storage
	shredResp, err := handleMpmMemory(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "shred",
		"params": map[string]interface{}{
			"id": memID,
		},
	})
	if err != nil {
		t.Fatalf("shred: %v", err)
	}
	if s, _ := shredResp.(map[string]interface{}); s == nil {
		t.Fatalf("shred returned nil response")
	}

	// resolve after shred must fail
	if _, err := handleMpmResolve(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"uri":  fmt.Sprintf("mpm://memory/%s", memID),
		"full": true,
	}); err == nil {
		t.Fatalf("resolve after shred succeeded — pointer cleanup failed")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func truncateTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ---------------------------------------------------------------------------
// G — Why surfaces legacy artifact_type='memory' evidence + confidence history
// ---------------------------------------------------------------------------
//
// The defect: before the legacy-widening fix in ListEvidence /
// QueryConfidenceHistory, evidence/confidence rows whose stored
// artifact_type was 'memory' (the old CLI default) were invisible to
// `mpm why <theory>` and `mpm why <decision>` even when the artifact_id
// matched. The brief's repro constructs such rows and asserts the Why
// surface reports them.
//
// We exercise the substrate paths directly (ListEvidence +
// QueryConfidenceHistory) rather than the full CLI, since the legacy
// widening is a substrate contract. The corresponding CLI test is in
// f15_why_regression_test.go.

func TestAcceptance_G_WhySurfacesLegacyEvidenceForTheory(t *testing.T) {
	dm := newTestIsolatedDM(t)

	// 1. create a theory (lives in memories with collection='theories')
	const theoryID = "acc-G-theory-legacy"
	const theoryContent = "acceptance G theory: legacy-shaped evidence must surface"
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, created_at, updated_at)
		VALUES (?, 'theories', ?, '[]', '{}',
		        CAST(strftime('%s','now') AS INTEGER),
		        CAST(strftime('%s','now') AS INTEGER))`,
		theoryID, theoryContent)
	if err != nil {
		t.Fatalf("seed theory: %v", err)
	}

	// 2. insert LEGACY-shaped evidence rows: artifact_type='memory'
	//    but linked to a theory id. These predate the typed-artifact
	//    migration; ListEvidence must widen the predicate to include
	//    them when the requested kind is 'theory'. Use ExecTracked
	//    (not raw SQLDB().Exec) so the write goes through the same
	//    connection pool as the read; raw Exec across separate pooled
	//    connections can race ahead of the read in some test setups.
	const evidenceCount = 3
	for i := 0; i < evidenceCount; i++ {
		_, err := dm.ExecTracked(`
			INSERT INTO evidence
				(id, artifact_id, artifact_type, type, source_group, strength, created_by, notes, created_at)
			VALUES
				(?, ?, 'memory', 'observation', 'legacy-cli', 0.5, 'acceptance-g', ?, CAST(strftime('%s','now') AS INTEGER))`,
			0, fmt.Sprintf("ev-legacy-theory-%d", i), theoryID, fmt.Sprintf("legacy evidence row %d", i))
		if err != nil {
			t.Fatalf("seed evidence %d: %v", i, err)
		}
	}

	// 3. insert LEGACY-shaped confidence_history rows. Trigger value
	// must be one of the canonical CHECK-allowed values; id must be
	// supplied because the column is PRIMARY KEY NOT NULL with no
	// DEFAULT.
	const historyCount = 4
	for i := 0; i < historyCount; i++ {
		_, err := dm.ExecTracked(`
			INSERT INTO confidence_history
				(id, artifact_id, artifact_type, confidence, evidence_count, trigger, computed_at)
			VALUES
				(?, ?, 'memory', 0.5, 1, ?, CAST(strftime('%s','now') AS INTEGER))`,
			0, fmt.Sprintf("ch-legacy-theory-%d", i), theoryID, confidenceHistoryTrigger)
		if err != nil {
			t.Fatalf("seed confidence_history %d: %v", i, err)
		}
	}

	// 4. substrate contract: ListEvidence widens artifact_type='memory'
	evRes, err := dm.ListEvidence(theoryID, "theory")
	if err != nil {
		t.Fatalf("ListEvidence(theory): %v", err)
	}
	evList, _ := evRes["evidence"].([]map[string]interface{})
	if len(evList) != evidenceCount {
		t.Fatalf("ListEvidence widened-mismatch: expected %d legacy rows surfaced for kind=theory; got %d", evidenceCount, len(evList))
	}

	// 5. substrate contract: QueryConfidenceHistory widens artifact_type='memory'
	histRes, err := dm.QueryConfidenceHistory(theoryID, "theory", 50)
	if err != nil {
		t.Fatalf("QueryConfidenceHistory(theory): %v", err)
	}
	// QueryConfidenceHistory returns `history` as []map[string]interface{},
	// NOT []interface{}. Asserting the wrong concrete type silently
	// returns nil — a classic Go test footgun. Verify shape explicitly.
	histRaw, ok := histRes["history"].([]map[string]interface{})
	if !ok {
		t.Fatalf("QueryConfidenceHistory(history) wrong concrete type: %T (want []map[string]interface{})", histRes["history"])
	}
	if len(histRaw) != historyCount {
		// Debug: dump what is actually in the table for this artifact
		rows, _ := dm.SQLDB().Query(`SELECT id, artifact_id, artifact_type, trigger FROM confidence_history WHERE artifact_id = ?`, theoryID)
		var dump strings.Builder
		for rows.Next() {
			var id, aid, atype, trig string
			rows.Scan(&id, &aid, &atype, &trig)
			dump.WriteString(fmt.Sprintf("  id=%s aid=%s atype=%s trig=%s\n", id, aid, atype, trig))
		}
		// Also try direct SQL bypassing QueryConfidenceHistory widening
		directRows, _ := dm.SQLDB().Query(
			`SELECT artifact_type, COUNT(*) FROM confidence_history WHERE artifact_id = ? GROUP BY artifact_type`, theoryID)
		var directDump strings.Builder
		for directRows.Next() {
			var atype string
			var cnt int
			directRows.Scan(&atype, &cnt)
			directDump.WriteString(fmt.Sprintf("  direct SQL atype=%s count=%d\n", atype, cnt))
		}
		t.Fatalf("QueryConfidenceHistory widened-mismatch: expected %d legacy rows; got %d.\nDB dump for %s:\n%sDirect group-by:\n%s",
			historyCount, len(histRaw), theoryID, dump.String(), directDump.String())
	}
}

func TestAcceptance_G_WhySurfacesLegacyEvidenceForDecision(t *testing.T) {
	dm := newTestIsolatedDM(t)

	const decisionID = "acc-G-decision-legacy"
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, created_at, updated_at)
		VALUES (?, 'decisions', ?, '[]', '{}',
		        CAST(strftime('%s','now') AS INTEGER),
		        CAST(strftime('%s','now') AS INTEGER))`,
		decisionID, "acceptance G decision: legacy evidence surfaces")
	if err != nil {
		t.Fatalf("seed decision: %v", err)
	}

	const evidenceCount = 2
	for i := 0; i < evidenceCount; i++ {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO evidence
				(id, artifact_id, artifact_type, type, source_group, strength, created_by, notes, created_at)
			VALUES
				(?, ?, 'memory', 'reversal', 'legacy-cli', 0.5, 'acceptance-g', ?, CAST(strftime('%s','now') AS INTEGER))`,
			fmt.Sprintf("ev-legacy-decision-%d", i), decisionID, fmt.Sprintf("decision legacy evidence %d", i))
		if err != nil {
			t.Fatalf("seed decision evidence %d: %v", i, err)
		}
	}

	evRes, err := dm.ListEvidence(decisionID, "decision")
	if err != nil {
		t.Fatalf("ListEvidence(decision): %v", err)
	}
	evList, _ := evRes["evidence"].([]map[string]interface{})
	if len(evList) != evidenceCount {
		t.Fatalf("decision legacy widening: expected %d rows; got %d", evidenceCount, len(evList))
	}
}

func TestAcceptance_G_WhySurfacesNewTypedWrites(t *testing.T) {
	dm := newTestIsolatedDM(t)

	const theoryID = "acc-G-theory-new"
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, created_at, updated_at)
		VALUES (?, 'theories', ?, '[]', '{}',
		        CAST(strftime('%s','now') AS INTEGER),
		        CAST(strftime('%s','now') AS INTEGER))`,
		theoryID, "acceptance G theory: typed writes work too")
	if err != nil {
		t.Fatalf("seed theory: %v", err)
	}

	// NEW typed writes — artifact_type='theory' directly
	const newCount = 2
	for i := 0; i < newCount; i++ {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO evidence
				(id, artifact_id, artifact_type, type, source_group, strength, created_by, notes, created_at)
			VALUES
				(?, ?, 'theory', 'observation', 'new-cli', 0.5, 'acceptance-g', ?, CAST(strftime('%s','now') AS INTEGER))`,
			fmt.Sprintf("ev-new-theory-%d", i), theoryID, fmt.Sprintf("new typed evidence %d", i))
		if err != nil {
			t.Fatalf("seed new evidence %d: %v", i, err)
		}
	}

	evRes, err := dm.ListEvidence(theoryID, "theory")
	if err != nil {
		t.Fatalf("ListEvidence(theory): %v", err)
	}
	evList, _ := evRes["evidence"].([]map[string]interface{})
	if len(evList) != newCount {
		t.Fatalf("new typed writes: expected %d rows; got %d", newCount, len(evList))
	}
}
