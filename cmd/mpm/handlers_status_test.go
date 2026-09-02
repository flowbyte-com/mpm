// handlers_status_test.go — alpha-5 D-5.1 / D-5.2 regression tests.
//
// D-5.1: `mpm status` previously queried theories with literal
// `status='resolved'`, which never matched (the lifecycle writes
// `proven` or `disproven`). The "resolved" counter was always 0.
//
// D-5.2: `countMemories` previously excluded only `deleted_at`, so
// expired memories (TTL'd out) continued to count toward memTotal.
// The dashboard overstated the active memory population.
//
// Both defects caused the status dashboard to systematically
// under-report (D-5.1) and over-report (D-5.2). The regression tests
// pin the fixed contract: proven+disproven theories appear in the
// resolved line; expired memories do NOT appear in total/LTM.

package main

import (
	"strconv"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// seedTheory inserts a theory memory with the given metadata.status so we
// can drive the counter from a known state without poking the FTS5 index
// through FullTextSearch.
func seedTheory(t *testing.T, dm *mpminternal.DatabaseManager, id, status string) {
	t.Helper()
	meta := `{"status":"` + status + `"}`
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, weight, created_at)
		VALUES (?, 'theories', ?, ?, 1, CAST(strftime('%s','now') AS INTEGER))
	`, id, "test theory: "+status, meta)
	if err != nil {
		t.Fatalf("seed theory %q: %v", id, err)
	}
}

// TestCountTheoriesByStatus_TerminalStatusesProvenDisproven is the
// alpha-5 D-5.1 regression. Pre-fix the dashboard used literal
// status='resolved' which never matches — proven + disproven theories
// were silently absent from the "resolved" line. The fix counts
// proven + disproven so the dashboard reflects the actual terminal
// population.
func TestCountTheoriesByStatus_TerminalStatusesProvenDisproven(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	// 2 pending, 3 proven, 1 disproven, 1 challenged (non-terminal).
	seedTheory(t, dm, "th-pend-1", "pending")
	seedTheory(t, dm, "th-pend-2", "pending")
	seedTheory(t, dm, "th-prov-1", "proven")
	seedTheory(t, dm, "th-prov-2", "proven")
	seedTheory(t, dm, "th-prov-3", "proven")
	seedTheory(t, dm, "th-disp-1", "disproven")
	seedTheory(t, dm, "th-chal-1", "challenged")

	pending, err := countTheoriesByStatus(dm, "pending")
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 2 {
		t.Errorf("pending theories: got %d, want 2", pending)
	}
	proven, err := countTheoriesByStatus(dm, "proven")
	if err != nil {
		t.Fatalf("count proven: %v", err)
	}
	if proven != 3 {
		t.Errorf("proven theories: got %d, want 3", proven)
	}
	disproven, err := countTheoriesByStatus(dm, "disproven")
	if err != nil {
		t.Fatalf("count disproven: %v", err)
	}
	if disproven != 1 {
		t.Errorf("disproven theories: got %d, want 1", disproven)
	}

	// Sanity: the literal status='resolved' MUST return 0 — the dashboard
	// never writes it and the acceptance pass previously queried against
	// it. This is the explicit no-op assertion that the bug was using the
	// wrong status enum value.
	resolved, err := countTheoriesByStatus(dm, "resolved")
	if err != nil {
		t.Fatalf("count resolved: %v", err)
	}
	if resolved != 0 {
		t.Errorf("'resolved' status is never written; expected 0, got %d", resolved)
	}

	// Dashboard contract: proven+disproven is what populates
	// d.theoryResolv (the dashboard's "resolved" line). 3+1 = 4.
	provenAgg, _ := countTheoriesByStatus(dm, "proven")
	disprovenAgg, _ := countTheoriesByStatus(dm, "disproven")
	if got := provenAgg + disprovenAgg; got != 4 {
		t.Errorf("dashboard resolved-line total: got %d, want 4", got)
	}
}

// TestCountMemories_AppliesExpireFilter is the alpha-5 D-5.2
// regression. Pre-fix countMemories only filtered deleted_at, so an
// expired memory (expires_at <= now) continued to count toward both
// total and LTM. The fix applies the canonical EXPIRE filter.
//
// The test asserts on deltas so it is robust to baseline-seeded
// memories (directives/lessons) which the production dashboard also
// counts.
func TestCountMemories_AppliesExpireFilter(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	// Capture baseline so we assert deltas (the production counter
	// includes baseline-seeded directives/lessons).
	totalBefore, err := countMemories(dm, "")
	if err != nil {
		t.Fatalf("baseline total: %v", err)
	}
	ltmBefore, err := countMemories(dm, "weight >= 10")
	if err != nil {
		t.Fatalf("baseline ltm: %v", err)
	}

	// 3 active memories (expires_at NULL → immortal).
	for i := 0; i < 3; i++ {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, metadata, weight, created_at, expires_at)
			VALUES (?, 'memories', ?, '{"weight":1}', 1, CAST(strftime('%s','now') AS INTEGER), NULL)
		`, "active-"+strconv.Itoa(i), "active content "+strconv.Itoa(i))
		if err != nil {
			t.Fatalf("seed active %d: %v", i, err)
		}
	}

	// 2 expired memories (expires_at = 0, i.e. 1970-01-01, always <= now).
	for i := 0; i < 2; i++ {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, metadata, weight, created_at, expires_at)
			VALUES (?, 'memories', ?, '{"weight":1}', 1, CAST(strftime('%s','now') AS INTEGER), 0)
		`, "expired-"+strconv.Itoa(i), "expired content "+strconv.Itoa(i))
		if err != nil {
			t.Fatalf("seed expired %d: %v", i, err)
		}
	}

	// 1 soft-deleted memory (deleted_at set) — should NOT count either way.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, weight, created_at, deleted_at)
		VALUES (?, 'memories', ?, '{"weight":1}', 1, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
	`, "deleted-1", "deleted content")
	if err != nil {
		t.Fatalf("seed deleted: %v", err)
	}

	total, err := countMemories(dm, "")
	if err != nil {
		t.Fatalf("countMemories total: %v", err)
	}
	if got := total - totalBefore; got != 3 {
		t.Errorf("countMemories total delta: got %d, want 3 (active only — expired/deleted excluded)", got)
	}

	// LTM filter: weight >= 10. Make one of the active memories LTM.
	_, err = dm.SQLDB().Exec(`UPDATE memories SET weight = 15 WHERE id = 'active-0'`)
	if err != nil {
		t.Fatalf("bump weight: %v", err)
	}
	ltm, err := countMemories(dm, "weight >= 10")
	if err != nil {
		t.Fatalf("countMemories ltm: %v", err)
	}
	if got := ltm - ltmBefore; got != 1 {
		t.Errorf("countMemories LTM delta: got %d, want 1 (only the bumped active-0)", got)
	}
}

// TestBuildStatusData_TheoryResolvedCountsProvenPlusDisproven is the
// integration-level D-5.1 regression: drive buildStatusData through a
// seeded theory population and confirm the resolved counter on the
// wire-shape thCounts reflects proven+disproven (not literal
// 'resolved'). Pre-fix this surfaced as dashboard "0 resolved" no
// matter how many theories were terminal.
func TestBuildStatusData_TheoryResolvedCountsProvenPlusDisproven(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	seedTheory(t, dm, "th-p1", "pending")
	seedTheory(t, dm, "th-p2", "pending")
	seedTheory(t, dm, "th-pr1", "proven")
	seedTheory(t, dm, "th-pr2", "proven")
	seedTheory(t, dm, "th-di1", "disproven")

	d := buildStatusData(dm, time.Now())

	if d.theoryTotal != 5 {
		t.Errorf("theoryTotal: got %d, want 5", d.theoryTotal)
	}
	if d.theoryPend != 2 {
		t.Errorf("theoryPend: got %d, want 2", d.theoryPend)
	}
	if d.theoryResolv != 3 {
		t.Errorf("theoryResolv (proven+disproven): got %d, want 3", d.theoryResolv)
	}
}