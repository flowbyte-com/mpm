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
	"fmt"
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

// seedDirective inserts a directive memory row so buildStatusData can
// be driven from a known directive population. The canonical
// identifier is `collection = 'directives'` (matches the MCP read
// path); the legacy `is_prime_directive` column is also written so
// the helper exercises both identification paths.
func seedDirective(t *testing.T, dm *mpminternal.DatabaseManager, id string) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, is_prime_directive, weight, created_at)
		VALUES (?, 'directives', ?, '{"is_prime_directive":1}', 1, 10, CAST(strftime('%s','now') AS INTEGER))
	`, id, "directive content "+id)
	if err != nil {
		t.Fatalf("seed directive %q: %v", id, err)
	}
}

// TestBuildStatusData_DirectivesExcludedFromMemoryTotals pins the
// 2026-09-16 fresh-profile fix: directives are stored in the
// `memories` table (collection='directives') but they must NOT
// inflate the dashboard's Memories row. Pre-fix the dashboard
// reported `Memories : 5 total | 5 LTM` on a pristine install
// because the canonical `countMemories` query counted ALL
// non-deleted memories including directives. The fix excludes
// directives from the memory count AND adds a separate `Directives`
// row to the dashboard.
//
// The test asserts on deltas (capturing the baseline-cognitive-bootstrap
// counts that NewDatabaseManager seeds) so it remains robust to
// additions/removals from the baseline set. It proves:
//
//   - memTotal does NOT grow when only directives are seeded
//     (i.e. directives do not inflate the memory totals)
//   - directives DOES grow by the seeded-directive count
//     (i.e. the Directives row reports the right count)
func TestBuildStatusData_DirectivesExcludedFromMemoryTotals(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	// Baseline (post-boot) counts — NewDatabaseManager runs the
	// baseline-cognitive-bootstrap seed which writes a handful of
	// canonical directives. The test must measure deltas against
	// this baseline, not absolute counts, so it remains stable if
	// the seed set is amended.
	dBaseline := buildStatusData(dm, time.Now())

	// Seed 5 additional directives (after the baseline) — mirrors
	// what an operator adding directives via `mpm call mpm_memory save`
	// would observe.
	const extraDirectives = 5
	for i := 0; i < extraDirectives; i++ {
		seedDirective(t, dm, fmt.Sprintf("extra-dir-%d", i))
	}

	d := buildStatusData(dm, time.Now())

	if got := d.directives - dBaseline.directives; got != extraDirectives {
		t.Errorf("directives delta: got %d, want %d (counting extraDirectives we just seeded)", got, extraDirectives)
	}

	// The whole point of this fix: adding directives must not
	// move the memTotal counter at all. Pre-fix, d.memTotal would
	// grow by `extraDirectives` and the dashboard would lie.
	if got := d.memTotal - dBaseline.memTotal; got != 0 {
		t.Errorf("memTotal delta: got %d, want 0 (directives must not inflate memory totals)", got)
	}
	if got := d.memLTM - dBaseline.memLTM; got != 0 {
		t.Errorf("memLTM delta: got %d, want 0 (directives must not inflate LTM totals)", got)
	}
}

// TestBuildStatusData_DirectivesAndMemoriesCountedSeparately verifies
// the same fix at a higher-cardinality boundary: when both directives
// AND genuine user memories are present, each is reported on its own
// dashboard row without bleeding into the other. This is the case the
// pre-fix dashboard got most confused about — a fresh install with
// later user memories would show the directive count lumped into the
// memory count and grow over time without the user realising.
func TestBuildStatusData_DirectivesAndMemoriesCountedSeparately(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	dBaseline := buildStatusData(dm, time.Now())

	// 3 extra directives + 7 ordinary memories (all on top of the
	// baseline-cognitive-bootstrap seed).
	const extraDirectives = 3
	const extraMemories = 7
	for i := 0; i < extraDirectives; i++ {
		seedDirective(t, dm, fmt.Sprintf("extra-dir-%d", i))
	}
	for i := 0; i < extraMemories; i++ {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, metadata, weight, created_at)
			VALUES (?, 'memories', ?, '{"weight":1}', 1, CAST(strftime('%s','now') AS INTEGER))
		`, fmt.Sprintf("extra-mem-%d", i), fmt.Sprintf("user memory %d", i))
		if err != nil {
			t.Fatalf("seed memory %d: %v", i, err)
		}
	}

	d := buildStatusData(dm, time.Now())

	if got := d.directives - dBaseline.directives; got != extraDirectives {
		t.Errorf("directives delta: got %d, want %d", got, extraDirectives)
	}
	if got := d.memTotal - dBaseline.memTotal; got != extraMemories {
		t.Errorf("memTotal delta: got %d, want %d (must count ordinary memories only, not directives)", got, extraMemories)
	}
	if got := d.memLTM - dBaseline.memLTM; got != 0 {
		t.Errorf("memLTM delta: got %d, want 0 (none of the seeded memories has weight >= 10)", got)
	}
}

// TestBuildStatusData_NullSafeDirectiveExclusion pins the 2026-09-16
// NULL-safety follow-up to bc7ce686. The pre-fix exclusion predicate
// `is_prime_directive != 1` is NOT NULL-safe in SQL: for a row with
// `is_prime_directive = NULL`, the comparison evaluates to UNKNOWN,
// so the whole AND predicate becomes UNKNOWN, and the row is
// silently excluded from memory counts.
//
// Imported, legacy, or manually-created memories can legally have
// `is_prime_directive = NULL` even when they are clearly ordinary
// memories (collection != 'directives'). The previous fix would
// hide them from the dashboard entirely.
//
// The canonical NULL-safe predicate is:
//
//	NOT (collection = 'directives' OR COALESCE(is_prime_directive, 0) = 1)
//
// so NULL in the flag column is treated as "not a directive".
// COALESCE(NULL, 0) = 0, and `0 = 1` is FALSE — the row passes
// through as an ordinary memory.
//
// This test seeds all three identification paths and asserts:
//
//   - ordinary memory with NULL flag IS counted (the bug surface)
//   - canonical directive (collection='directives') IS NOT counted
//   - legacy directive (flag=1, non-directives collection) IS NOT counted
//   - directive count itself includes BOTH canonical and legacy
func TestBuildStatusData_NullSafeDirectiveExclusion(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	dBaseline := buildStatusData(dm, time.Now())

	// (1) Ordinary memory with NULL is_prime_directive — the
	// canonical bug surface. collection != 'directives' makes it
	// obviously ordinary; the NULL flag must NOT hide it.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, is_prime_directive, weight, created_at)
		VALUES (?, 'default', ?, '{"weight":1}', NULL, 1, CAST(strftime('%s','now') AS INTEGER))
	`, "ordinary-null", "ordinary memory with NULL flag")
	if err != nil {
		t.Fatalf("seed ordinary-null: %v", err)
	}

	// (2) Canonical directive — collection='directives' with flag=NULL.
	// Should be excluded from memTotal, counted in directives.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, is_prime_directive, weight, created_at)
		VALUES (?, 'directives', ?, '{"is_prime_directive":1}', NULL, 10, CAST(strftime('%s','now') AS INTEGER))
	`, "canonical-null", "canonical directive with NULL flag")
	if err != nil {
		t.Fatalf("seed canonical-null: %v", err)
	}

	// (3) Legacy directive — collection != 'directives', flag=1.
	// Should be excluded from memTotal, counted in directives.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, is_prime_directive, weight, created_at)
		VALUES (?, 'legacy_collection', ?, '{"is_prime_directive":1}', 1, 10, CAST(strftime('%s','now') AS INTEGER))
	`, "legacy-flag", "legacy directive with flag=1, non-directives collection")
	if err != nil {
		t.Fatalf("seed legacy-flag: %v", err)
	}

	d := buildStatusData(dm, time.Now())

	// memTotal must grow by exactly 1 — the ordinary-null row. The
	// canonical-null and legacy-flag rows must NOT contribute.
	if got := d.memTotal - dBaseline.memTotal; got != 1 {
		t.Errorf("memTotal delta: got %d, want 1 (the NULL-flag ordinary memory must count; directives must be excluded)", got)
	}

	// memLTM also grows by 1 (the ordinary row, weight=1 → not LTM; but
	// the canonical-null has weight=10 which IS LTM. So actually:
	// ordinary-null weight=1 → not LTM (NOT counted in LTM)
	// canonical-null weight=10 → directive (excluded from LTM)
	// legacy-flag weight=10 → directive (excluded from LTM)
	// So memLTM delta should be 0. The bug fix has to not flip this.
	if got := d.memLTM - dBaseline.memLTM; got != 0 {
		t.Errorf("memLTM delta: got %d, want 0 (ordinary has weight=1, directives excluded regardless of flag/collection)", got)
	}

	// directives must grow by exactly 2 — canonical-null + legacy-flag.
	if got := d.directives - dBaseline.directives; got != 2 {
		t.Errorf("directives delta: got %d, want 2 (canonical + legacy; both identification paths must contribute)", got)
	}
}

// TestCountMemories_NullSafeDirectiveExclusion covers the same
// NULL-safety contract at the countMemories boundary specifically —
// independent of the buildStatusData aggregation layer. This is
// the regression a future refactor of countMemories cannot
// silently reintroduce.
func TestCountMemories_NullSafeDirectiveExclusion(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	// Capture baseline BEFORE seeding so the assertion measures the
	// delta attributable to our four seeded rows.
	baselineOrd, err := countMemories(dm, "")
	if err != nil {
		t.Fatalf("baseline memTotal: %v", err)
	}
	baselineDir, err := countDirectives(dm)
	if err != nil {
		t.Fatalf("baseline directives: %v", err)
	}

	// Seed the four canonical rows that exercise every combination
	// of directive-identification pair.
	rows := []struct {
		id      string
		coll    string
		primeV  interface{}
		comment string
	}{
		{"ord-null", "default", nil, "ordinary memory, NULL flag (the bug surface)"},
		{"ord-zero", "default", 0, "ordinary memory, flag=0"},
		{"dir-canon", "directives", nil, "canonical directive, NULL flag"},
		{"dir-legacy", "default", 1, "legacy directive, flag=1, non-directives collection"},
	}
	for _, r := range rows {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, metadata, is_prime_directive, weight, created_at)
			VALUES (?, ?, ?, '{}', ?, 1, CAST(strftime('%s','now') AS INTEGER))
		`, r.id, r.coll, r.comment, r.primeV)
		if err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}

	// memTotal must grow by 2 (ord-null + ord-zero). The two
	// directive rows (dir-canon, dir-legacy) must NOT count toward
	// memTotal regardless of which identification criterion matched.
	afterOrd, err := countMemories(dm, "")
	if err != nil {
		t.Fatalf("after memTotal: %v", err)
	}
	if got := afterOrd - baselineOrd; got != 2 {
		t.Errorf("countMemories delta: got %d, want 2 (ord-null + ord-zero; the NULL-flag ordinary row must count, directives must be excluded)", got)
	}

	// directives count must grow by 2 (dir-canon + dir-legacy).
	afterDir, err := countDirectives(dm)
	if err != nil {
		t.Fatalf("after directives: %v", err)
	}
	if got := afterDir - baselineDir; got != 2 {
		t.Errorf("countDirectives delta: got %d, want 2 (dir-canon + dir-legacy; both identification paths must contribute)", got)
	}
}

// TestMemoryStats_StatusAndInfoAgreeOnNullSafeCount pins the
// semantic alignment between `mpm status` (countMemories),
// `mpm info` (GetMemoryStats["active"]), and the canonical
// health-check field memories_active. All three queries must
// return the same count for the same database — they use the
// same NULL-safe directive-exclusion predicate, so they can
// never drift on ordinary-memory accounting.
//
// The test seeds a NULL-flag ordinary memory plus several
// directive variants and asserts the three surfaces agree.
func TestMemoryStats_StatusAndInfoAgreeOnNullSafeCount(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	// Seed a NULL-flag ordinary memory plus one of each directive
	// variant. The NULL-flag row is the regression target — it
	// must show up in all three memory counts.
	seeds := []struct {
		id     string
		coll   string
		prime  interface{}
		weight int
	}{
		// Ordinary with NULL flag (the bug surface).
		{"ord-null-1", "default", nil, 1},
		{"ord-null-2", "default", nil, 5},
		// Canonical directive (collection + NULL flag).
		{"dir-canon-null", "directives", nil, 10},
		// Legacy directive (flag=1, non-directives collection).
		{"dir-legacy", "default", 1, 10},
	}
	for _, s := range seeds {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, metadata, is_prime_directive, weight, created_at)
			VALUES (?, ?, 'content-'||?, '{}', ?, ?, CAST(strftime('%s','now') AS INTEGER))
		`, s.id, s.coll, s.id, s.prime, s.weight)
		if err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}

	// Surface 1: countMemories("") — what `mpm status` renders as MemTotal.
	memTotal, err := countMemories(dm, "")
	if err != nil {
		t.Fatalf("countMemories: %v", err)
	}

	// Surface 2: GetMemoryStats["active"] — what `mpm info` renders as "active".
	stats, err := dm.GetMemoryStats()
	if err != nil {
		t.Fatalf("GetMemoryStats: %v", err)
	}
	infoActive, ok := stats["active"].(int)
	if !ok {
		t.Fatalf("GetMemoryStats[\"active\"] missing or wrong type: %T %v", stats["active"], stats["active"])
	}

	// Surface 3: memories_active health-check field (run via the
	// same code path HealthCheck uses).
	healthStats, err := dm.HealthCheck()
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	memActive, ok := healthStats["memories_active"].(int64)
	if !ok {
		t.Fatalf("HealthCheck[memories_active] missing or wrong type: %T %v", healthStats["memories_active"], healthStats["memories_active"])
	}

	// All three must agree on the ordinary-memory count (the two
	// ord-null rows; the two directive rows must be excluded
	// from each surface). The exact number depends on what the
	// baseline-cognitive-bootstrap seed wrote (it does NOT
	// include NULL-flag ordinary memories, so the deltas from
	// baseline are deterministic).
	if memTotal != infoActive {
		t.Errorf("countMemories (%d) disagrees with GetMemoryStats[active] (%d); NULL-flag ordinary memories must count identically on both surfaces", memTotal, infoActive)
	}
	if int64(memTotal) != memActive {
		t.Errorf("countMemories (%d) disagrees with HealthCheck[memories_active] (%d); all three memory-count surfaces must share the canonical NULL-safe predicate", memTotal, memActive)
	}

	// Stronger assertion: every surface must reflect the two
	// ord-null seeds as ordinary memories. We can't pin absolute
	// numbers (baseline seed varies), but we can pin the delta:
	// build a "counted" baseline first, then re-count after.
	preOrd, err := countMemories(dm, "")
	if err != nil {
		t.Fatalf("preOrd: %v", err)
	}
	// Insert another NULL-flag ordinary row.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, is_prime_directive, weight, created_at)
		VALUES (?, 'default', 'extra', '{}', NULL, 1, CAST(strftime('%s','now') AS INTEGER))
	`, "ord-null-extra")
	if err != nil {
		t.Fatalf("seed ord-null-extra: %v", err)
	}
	postOrd, err := countMemories(dm, "")
	if err != nil {
		t.Fatalf("postOrd: %v", err)
	}
	if got := postOrd - preOrd; got != 1 {
		t.Errorf("NULL-flag ordinary memory delta: got %d, want 1 (every memory-count surface must use the NULL-safe predicate)", got)
	}
}
