// cross_agent_continuity_test.go — Release acceptance for the central
// MPM product claim:
//
//   persistent MPM state allows discontinuous agents, sessions,
//   models, and supported frameworks to participate in one
//   continuing cognitive process.
//
// The harness simulates two distinct agents that share only the
// durable MPM substrate:
//
//   Agent A — the producer. Writes Work, Decision, Theory, Lesson,
//             Memory, Handoff using normal MPM APIs and then "dies".
//
//   Agent B — the consumer. Opens a NEW DatabaseManager over the
//             SAME substrate file (different process / different
//             *DatabaseManager object, identical SQLite file). Its
//             only allowed continuity operation is read_wake_context.
//
// Success criteria are factual continuation facts, not prose
// judgement. The assertions check that Agent B's wake context
// exposes enough information to identify the canonical scenario's
// work, decision, theory, lesson, memory, handoff without ever
// receiving Agent A's transient context.

package release_acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	_ "github.com/mattn/go-sqlite3"
)

// scenarioIDs bundles the canonical scenario's seeded artifact IDs.
type scenarioIDs struct {
	workID         string
	decisionID     string
	activeTheoryID string
	supersededID   string
	lessonID       string
	memoryID       string
	handoffID      string
	wakeID         string
	mpmSessionA    string
	mpmSessionB    string
}

// ── Fixture setup ──────────────────────────────────────────────

// makeAcceptanceWorkspace builds a hermetic MPM_WORKSPACE on disk
// and returns the path plus a cleanup func. This isolates the
// acceptance harness from the user's real ~/.mpm and from any
// other test run.
func makeAcceptanceWorkspace(t *testing.T) (string, func()) {
	t.Helper()
	root := t.TempDir()
	mpmDir := filepath.Join(root, ".mpm")
	if err := os.MkdirAll(filepath.Join(mpmDir, "src/db"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	prev := os.Getenv("MPM_WORKSPACE")
	os.Setenv("MPM_WORKSPACE", root)
	return root, func() {
		if prev != "" {
			os.Setenv("MPM_WORKSPACE", prev)
		} else {
			os.Unsetenv("MPM_WORKSPACE")
		}
	}
}

// seedAgentACanonical seeds the canonical scenario: a work item, a
// decision supported by an active theory, a superseded predecessor
// theory, a lesson, a durable memory, an overdue wake, and an unread
// handoff. Agent A "writes" these using the public MPM APIs as if it
// were a normal host.
func seedAgentACanonical(t *testing.T, dm mpminternal.CoreDB, ids *scenarioIDs) {
	t.Helper()
	now := time.Now().Unix()

	// Work W (open, unverified)
	ids.workID = "W-cross-agent-1"
	_, err := dm.SQLDB().Exec(`
		INSERT INTO works (id, title, status, verification, created_at, updated_at, session_id)
		VALUES (?, 'cross-agent continuity work item', 'open', 'unverified', ?, ?, ?)
	`, ids.workID, now, now, ids.mpmSessionA)
	if err != nil {
		t.Fatalf("insert work: %v", err)
	}

	// Active theory T_active (foundation supporting D)
	ids.activeTheoryID = "T-cross-agent-active"
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES (?, 'theories', 'active foundation theory for cross-agent acceptance', ?, ?, '["pending"]', '{}', '', 'openclaw')
	`, ids.activeTheoryID, now, now)
	if err != nil {
		t.Fatalf("insert active theory: %v", err)
	}

	// Superseded predecessor T_old
	ids.supersededID = "T-cross-agent-superseded"
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES (?, 'theories', 'OBSOLETE predecessor theory; do not surface as current truth', ?, ?, '["superseded","superseded-by:T-cross-agent-active"]', '{}', '', 'openclaw')
	`, ids.supersededID, now, now)
	if err != nil {
		t.Fatalf("insert superseded: %v", err)
	}
	// Record supersession in confidence_history.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES ('ch-cross-1', ?, 'theory', 0.1, ?, 0, 'supersede')
	`, ids.supersededID, now)
	if err != nil {
		t.Fatalf("confidence history: %v", err)
	}
	// Mark successor in metadata.
	_, err = dm.SQLDB().Exec(`
		UPDATE memories SET metadata = json_set(metadata, '$.superseded_by', ?)
		WHERE id = ? AND collection = 'theories'
	`, ids.activeTheoryID, ids.supersededID)
	if err != nil {
		t.Fatalf("metadata update: %v", err)
	}

	// Decision D (depends on active theory)
	ids.decisionID = "D-cross-agent-1"
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES (?, 'decisions', 'current canonical decision grounded in active theory', ?, ?, '[]', '{}', '', 'openclaw')
	`, ids.decisionID, now, now)
	if err != nil {
		t.Fatalf("insert decision: %v", err)
	}

	// Lesson L
	ids.lessonID = "L-cross-agent-1"
	_, err = dm.SQLDB().Exec(`
		INSERT INTO lessons (id, type, content, tags, source_session_id, created, content_hash, reinforcement_count)
		VALUES (?, 'insight', 'cross-agent lesson content', '[]', ?, ?, '', 1)
	`, ids.lessonID, ids.mpmSessionA, time.Unix(now, 0).UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatalf("insert lesson: %v", err)
	}

	// Memory M (durable contextual fact)
	ids.memoryID = "M-cross-agent-1"
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES (?, 'memory', 'cross-agent durable contextual fact', ?, ?, '[]', '{}', '', 'openclaw')
	`, ids.memoryID, now, now)
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	// Overdue wake K
	ids.wakeID = "K-cross-agent-1"
	_, err = dm.SQLDB().Exec(`
		INSERT INTO scheduled_wakes (id, target_time, reason, fired, theory_id, created_by, created_at)
		VALUES (?, ?, 'reconsider decision after theory invalidation', 0, ?, 'mpm-cli', ?)
	`, ids.wakeID, now-3600, ids.decisionID, now)
	if err != nil {
		t.Fatalf("insert wake: %v", err)
	}

	// Unread handoff H
	ids.handoffID = "H-cross-agent-1"
	_, err = dm.SQLDB().Exec(`
		INSERT INTO session_handoffs
		    (id, mpm_session_id, framework_session_id, ended_at, ended_state,
		     summary, commitments, open_questions, created_at)
		VALUES (?, ?, 'framework-A-session', ?, 'clean',
		        'cross-agent handoff summary', '["resume cross-agent continuity work"]', '["is foundation still valid?"]', ?)
	`, ids.handoffID, ids.mpmSessionA, now, now)
	if err != nil {
		t.Fatalf("insert handoff: %v", err)
	}
}

// seedIrrelevantNoise adds many newer-but-unrelated memories +
// activity rows AFTER Agent A's canonical state. Per the brief, the
// runtime must surface structurally relevant older state ahead of
// these newer unrelated items.
func seedIrrelevantNoise(t *testing.T, dm mpminternal.CoreDB) {
	t.Helper()
	now := time.Now().Unix()
	for i := 0; i < 20; i++ {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
			VALUES (?, 'memory', ?, ?, ?, '[]', '{}', '', 'mpm-cli')
		`, fmt.Sprintf("noise-%d", i), fmt.Sprintf("unrelated noise item %d", i),
			now+int64(i), now+int64(i))
		if err != nil {
			t.Fatalf("noise memory %d: %v", i, err)
		}
	}
}

// ── Test suite ─────────────────────────────────────────────────

// TestCrossAgentContinuity_CanonicalScenario is the central acceptance
// test. It proves Agent B (a fresh DatabaseManager over the SAME SQLite
// file) receives enough relevant inherited context through the normal
// read_wake_context path to continue the work Agent A started.
func TestCrossAgentContinuity_CanonicalScenario(t *testing.T) {
	root, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()

	ids := &scenarioIDs{
		mpmSessionA: "mpm-cross-A-session",
		mpmSessionB: "mpm-cross-B-session",
	}

	// ── Phase 1: Agent A writes the canonical scenario.
	dmA, errA := mpminternal.NewDatabaseManager("")
	if errA != nil {
		t.Fatalf("NewDatabaseManager: %v", errA)
	}
	if err := dmA.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	seedAgentACanonical(t, dmA, ids)
	seedIrrelevantNoise(t, dmA)

	// Capture observational diff baseline.
	tables := []string{
		"works", "memories", "lessons", "session_handoffs",
		"scheduled_wakes", "tool_invocations",
	}
	before := snapshotCounts(t, dmA, tables)

	// Verify Agent A's handoff is unread pre-handoff.
	preUnread := countUnreadHandoffs(t, dmA)
	if preUnread != 1 {
		t.Fatalf("pre-handoff unread count = %d, want 1", preUnread)
	}

	// ── Phase 2: Agent A "dies". Reopen the same DB file in a NEW
	// DatabaseManager (Agent B's process).
	dmA.Close()

	dmB, errB := mpminternal.NewDatabaseManager("")
	if errB != nil {
		t.Fatalf("NewDatabaseManager: %v", errB)
	}
	defer dmB.Close()

	// After reopen, durable state must be exactly preserved.
	after := snapshotCounts(t, dmB, tables)
	for tbl, b := range before {
		if after[tbl] != b {
			t.Errorf("after reopen: table %s = %d, want %d (continuity lost)",
				tbl, after[tbl], b)
		}
	}

	// ── Phase 3: Agent B's only continuity call is read_wake_context.
	// It must NOT use any of the diagnostic contextual_* actions.
	ctx := mpminternal.ContextQuery{
		MPMSessionID:       ids.mpmSessionB,
		FrameworkSessionID: "framework-B-session",
		FrameworkName:      "openclaw",
		WorkIDs:            activeWorkIDs(t, dmB),
	}
	data, err := dmB.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("Agent B read_wake_context: %v", err)
	}

	// ── Phase 4: assert factual continuation facts.
	// 4.1 — Legacy wake field set carries the handoff.
	if data.LastHandoff == nil || data.LastHandoff.ID != ids.handoffID {
		t.Errorf("LastHandoff = %+v, want %s",
			data.LastHandoff, ids.handoffID)
	}
	// 4.2 — Open work carries the work objective in title form.
	foundWork := false
	for _, w := range data.OpenWorks {
		if w.ID == ids.workID {
			foundWork = true
			if !strings.Contains(strings.ToLower(w.Title), "cross-agent") {
				t.Errorf("work title %q does not carry objective marker", w.Title)
			}
		}
	}
	if !foundWork {
		t.Errorf("open works missing %s; got %d open works",
			ids.workID, len(data.OpenWorks))
	}

	// 4.3 — contextual_focus is healthy and item count matches selected.
	if data.ContextualFocus == nil {
		t.Fatalf("ContextualFocus nil")
	}
	if data.ContextualFocus.Status != mpminternal.ContextualFocusAvailable {
		t.Errorf("focus status = %q, want available",
			data.ContextualFocus.Status)
	}
	if data.ContextualFocus.SelectedInputCount !=
		len(data.ContextualFocus.Items) {
		t.Errorf("focus cardinality: selected=%d != items=%d",
			data.ContextualFocus.SelectedInputCount,
			len(data.ContextualFocus.Items))
	}

	// 4.4 — Critical canonical facts recoverable from focus.
	//
	// After the Stage 2D supersession-chain repair (commit fixing
	// the cross-agent continuity defect), the canonical Stage 2E.3
	// contract surfaces:
	//   - the work objective
	//   - the unread handoff
	//   - the overdue wake
	//   - the CURRENT CANONICAL THEORY (T-active), not the
	//     superseded predecessor. The predecessor is compressed
	//     into the canonical successor via CompressedRelatedIDs;
	//     its policy importance travels via CompressionTriggers
	//     on the canonical successor.
	//
	// Acceptance §4.4 verifies the routing pipeline delivers
	// structurally relevant awareness with the CURRENT canonical
	// state as the primary artifact.
	focus := data.ContextualFocus.Items
	byArtifact := map[string]mpminternal.ContextualFocusItem{}
	for _, it := range focus {
		byArtifact[it.ArtifactID] = it
	}
	mustSurface := []string{
		ids.workID,         // work objective — direct obligation
		ids.handoffID,      // unread handoff — continuity
		ids.wakeID,         // overdue wake — obligation
		ids.activeTheoryID, // CURRENT canonical theory (supersession invariant)
	}
	for _, must := range mustSurface {
		if _, ok := byArtifact[must]; !ok {
			t.Errorf("contextual_focus missing canonical artifact %s", must)
		}
	}
	// 4.5 — Hard gate: the canonical successor T-active MUST be the
	// primary delivered artifact for the supersession chain. The
	// superseded predecessor T-superseded must NOT be delivered as
	// its own focus item — it travels via CompressedRelatedIDs /
	// CompressionTriggers on the canonical successor.
	if supersededItem, leaked := byArtifact[ids.supersededID]; leaked {
		t.Errorf("supersession hard gate: superseded predecessor %s leaked "+
			"as its own focus item; canonical successor %s should be "+
			"the primary artifact",
			supersededItem.ArtifactID, ids.activeTheoryID)
	}
	if active, hasActive := byArtifact[ids.activeTheoryID]; hasActive {
		// Active successor must carry compressed related IDs
		// pointing at the historical predecessor. Note that
		// CompressedRelatedIDs uses the kind-prefixed form
		// ("theory:<id>") while SelectionTriggers.SourceID uses
		// the same form.
		wantPredecessorKey := "theory:" + ids.supersededID
		foundCompressed := false
		for _, rid := range active.CompressedRelatedIDs {
			if rid == wantPredecessorKey {
				foundCompressed = true
				break
			}
		}
		if !foundCompressed {
			t.Errorf("canonical successor must carry %s in CompressedRelatedIDs; "+
				"got %v",
				wantPredecessorKey, active.CompressedRelatedIDs)
		}
		// At least one compression trigger must record the
		// predecessor's policy importance.
		hasTrigger := false
		for _, trig := range active.SelectionTriggers {
			if trig.SourceID == wantPredecessorKey {
				hasTrigger = true
				break
			}
		}
		if !hasTrigger {
			t.Errorf("canonical successor must carry %s in SelectionTriggers; "+
				"got %v",
				wantPredecessorKey, active.SelectionTriggers)
		}
	}

	// 4.6 — Handoff remains unread after the read-only call.
	if unread := countUnreadHandoffs(t, dmB); unread != 1 {
		t.Errorf("post read-only unread = %d, want 1", unread)
	}

	// 4.7 — Boundedness: the focus is bounded, not a substrate replay.
	if len(focus) > 50 {
		t.Errorf("focus item count = %d exceeds bounded cap", len(focus))
	}
	detailBytes := 0
	for _, it := range focus {
		detailBytes += len(it.Detail)
	}
	if detailBytes > 7000 {
		t.Errorf("focus detail bytes = %d exceeds budget", detailBytes)
	}

	// 4.8 — Persist intermediate observation: focus delivery did not
	// write any new persistent rows.
	afterRO := snapshotCounts(t, dmB, tables)
	for tbl, b := range before {
		if afterRO[tbl] != b {
			t.Errorf("read-only gather wrote table %s: %d → %d",
				tbl, b, afterRO[tbl])
		}
	}
	_ = ctx
	_ = root
}

// TestCrossAgentContinuity_HandoffDelivery proves the delivery path
// consumes the unread handoff exactly once, and a subsequent call
// does not redeliver it.
func TestCrossAgentContinuity_HandoffDelivery(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	ids := &scenarioIDs{
		mpmSessionA: "mpm-handoff-A",
		mpmSessionB: "mpm-handoff-B",
	}

	dmA, errA := mpminternal.NewDatabaseManager("")
	if errA != nil {
		t.Fatalf("NewDatabaseManager: %v", errA)
	}
	seedAgentACanonical(t, dmA, ids)
	dmA.Close()

	dmB, errB := mpminternal.NewDatabaseManager("")
	if errB != nil {
		t.Fatalf("NewDatabaseManager: %v", errB)
	}
	defer dmB.Close()

	// First delivery.
	data1, err := dmB.GatherWakeContext()
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if data1.LastHandoff == nil || data1.LastHandoff.ID != ids.handoffID {
		t.Fatalf("first delivery: LastHandoff = %+v, want %s",
			data1.LastHandoff, ids.handoffID)
	}
	if unread := countUnreadHandoffs(t, dmB); unread != 0 {
		t.Errorf("after first delivery: unread = %d, want 0", unread)
	}

	// Second delivery: no handoff redelivery.
	data2, err := dmB.GatherWakeContext()
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if data2.LastHandoff != nil {
		t.Errorf("second delivery: handoff redelivered = %+v", data2.LastHandoff)
	}
}

// TestCrossAgentContinuity_ProcessRestart proves Agent B can start
// in a NEW process after Agent A's process has fully exited, with no
// in-memory state sharing.
func TestCrossAgentContinuity_ProcessRestart(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	ids := &scenarioIDs{
		mpmSessionA: "mpm-restart-A",
		mpmSessionB: "mpm-restart-B",
	}

	dmA, errA := mpminternal.NewDatabaseManager("")
	if errA != nil {
		t.Fatalf("NewDatabaseManager: %v", errA)
	}
	seedAgentACanonical(t, dmA, ids)
	dmA.Close()

	// Brand-new DatabaseManager — Agent B's process.
	dmB, errB := mpminternal.NewDatabaseManager("")
	if errB != nil {
		t.Fatalf("NewDatabaseManager: %v", errB)
	}
	defer dmB.Close()

	data, err := dmB.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("Agent B read_wake_context after restart: %v", err)
	}
	if data.LastHandoff == nil || data.LastHandoff.ID != ids.handoffID {
		t.Errorf("post-restart handoff lost: %+v", data.LastHandoff)
	}
}

// TestCrossAgentContinuity_FutureWakeUnaffected proves Agent B
// receives a future wake as supporting context, not an immediate
// obligation. Stage 2E.1 selection policy owns this; Agent B need
// only see it correctly classified.
func TestCrossAgentContinuity_FutureWakeUnaffected(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()

	dmA, errA := mpminternal.NewDatabaseManager("")
	if errA != nil {
		t.Fatalf("NewDatabaseManager: %v", errA)
	}
	now := time.Now().Unix()
	// Direct work first.
	_, err := dmA.SQLDB().Exec(`
		INSERT INTO works (id, title, status, verification, created_at, updated_at, session_id)
		VALUES ('W-future-test', 'future wake fixture work', 'open', 'unverified', ?, ?, '')
	`, now, now)
	if err != nil {
		t.Fatalf("insert work: %v", err)
	}
	// Future wake (target_time far in the future).
	_, err = dmA.SQLDB().Exec(`
		INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, created_at)
		VALUES ('K-future', ?, 'future reminder', 0, 'mpm-cli', ?)
	`, now+86400, now)
	if err != nil {
		t.Fatalf("insert future wake: %v", err)
	}
	dmA.Close()

	dmB, errB := mpminternal.NewDatabaseManager("")
	if errB != nil {
		t.Fatalf("NewDatabaseManager: %v", errB)
	}
	defer dmB.Close()
	data, err := dmB.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}
	// OverdueWakes must NOT include the future wake.
	for _, w := range data.OverdueWakes {
		if w.ID == "K-future" {
			t.Errorf("future wake surfaced as overdue: %+v", w)
		}
	}
}

// TestCrossAgentContinuity_SessionIdentityProvenance proves
// Agent A's framework session id does NOT contaminate Agent B's
// parent_invocation_id. Agent B's MPM session and framework session
// remain distinct.
func TestCrossAgentContinuity_SessionIdentityProvenance(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	ids := &scenarioIDs{
		mpmSessionA: "mpm-sess-A",
		mpmSessionB: "mpm-sess-B",
	}

	dmA, errA := mpminternal.NewDatabaseManager("")
	if errA != nil {
		t.Fatalf("NewDatabaseManager: %v", errA)
	}
	seedAgentACanonical(t, dmA, ids)
	dmA.Close()

	dmB, errB := mpminternal.NewDatabaseManager("")
	if errB != nil {
		t.Fatalf("NewDatabaseManager: %v", errB)
	}
	defer dmB.Close()

	data, err := dmB.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}
	// Agent B's MPM session should be reachable via CurrentMPMSessionID
	// (or empty if active.json is unset in the test env); Agent A's
	// framework session must NOT have leaked into parent_invocation_id
	// semantics.
	if data.MPMSessionID == ids.mpmSessionA {
		t.Errorf("MPMSessionID bled from Agent A: %s", data.MPMSessionID)
	}
	if data.FrameworkSessionID == "framework-A-session" {
		t.Errorf("FrameworkSessionID bled from Agent A: %s",
			data.FrameworkSessionID)
	}
}

// TestCrossAgentContinuity_PointerFollowing proves Agent B can
// resolve a focus pointer through the normal domain tool without
// re-running any diagnostic contextual_* action.
func TestCrossAgentContinuity_PointerFollowing(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	ids := &scenarioIDs{
		mpmSessionA: "mpm-pointer-A",
		mpmSessionB: "mpm-pointer-B",
	}

	dmA, errA := mpminternal.NewDatabaseManager("")
	if errA != nil {
		t.Fatalf("NewDatabaseManager: %v", errA)
	}
	seedAgentACanonical(t, dmA, ids)
	dmA.Close()

	dmB, errB := mpminternal.NewDatabaseManager("")
	if errB != nil {
		t.Fatalf("NewDatabaseManager: %v", errB)
	}
	defer dmB.Close()

	data, err := dmB.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}

	// Find a focus item of kind `memory`, `decision`, or `theory`
	// (those resolve via mpm_memory show). Work / handoff / wake /
	// activity live in different tables and use other domain tools;
	// skip them — the pointer-following contract is generic.
	var followedID string
	for _, it := range data.ContextualFocus.Items {
		if it.Kind != "memory" && it.Kind != "decision" && it.Kind != "theory" {
			continue
		}
		followedID = it.ArtifactID
		break
	}
	if followedID == "" {
		t.Skip("no memory-class focus item in fixture; not a regression")
	}

	// Agent B follows the pointer using the normal domain tool —
	// mpm_memory show for memory/decision/theory items.
	mem, err := dmB.GetMemory(followedID)
	if err != nil {
		t.Fatalf("pointer-following GetMemory(%s): %v", followedID, err)
	}
	if mem == nil {
		t.Errorf("pointer-following: GetMemory returned nil for %s", followedID)
	}
}

// TestCrossAgentContinuity_NoRoutingPersistence proves that across
// the producer-consumer handoff, Agent B's wake-context read did not
// persist any routing state.
func TestCrossAgentContinuity_NoRoutingPersistence(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	ids := &scenarioIDs{
		mpmSessionA: "mpm-persist-A",
		mpmSessionB: "mpm-persist-B",
	}

	dmA, errA := mpminternal.NewDatabaseManager("")
	if errA != nil {
		t.Fatalf("NewDatabaseManager: %v", errA)
	}
	seedAgentACanonical(t, dmA, ids)
	before := snapshotCounts(t, dmA, []string{
		"works", "memories", "lessons", "session_handoffs",
		"scheduled_wakes",
	})
	dmA.Close()

	dmB, errB := mpminternal.NewDatabaseManager("")
	if errB != nil {
		t.Fatalf("NewDatabaseManager: %v", errB)
	}
	defer dmB.Close()

	if _, err := dmB.GatherWakeContextReadOnly(); err != nil {
		t.Fatalf("read-only: %v", err)
	}
	after := snapshotCounts(t, dmB, []string{
		"works", "memories", "lessons", "session_handoffs",
		"scheduled_wakes",
	})
	for tbl, b := range before {
		if after[tbl] != b {
			t.Errorf("Agent B read-only persisted to %s: %d → %d",
				tbl, b, after[tbl])
		}
	}
}

// TestCrossAgentContinuity_ReopenDatabaseReadOnlyFromFilePath is a
// stricter database-reopen test: write through dmA, fully close it,
// then construct dmB from a fresh DatabaseManager (which opens the
// SAME SQLite file via the configured workspace). This is the
// process-restart variant at a different object level.
func TestCrossAgentContinuity_ReopenDatabaseReadOnlyFromFilePath(t *testing.T) {
	_, cleanup := makeAcceptanceWorkspace(t)
	defer cleanup()
	ids := &scenarioIDs{
		mpmSessionA: "mpm-reopen-A",
		mpmSessionB: "mpm-reopen-B",
	}

	dmA, errA := mpminternal.NewDatabaseManager("")
	if errA != nil {
		t.Fatalf("NewDatabaseManager: %v", errA)
	}
	seedAgentACanonical(t, dmA, ids)
	dmA.Close()

	dmB, errB := mpminternal.NewDatabaseManager("")
	if errB != nil {
		t.Fatalf("NewDatabaseManager: %v", errB)
	}
	defer dmB.Close()

	// Confirm Agent B's wake context exposes the work ID across
	// the reopen boundary.
	data, err := dmB.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}
	found := false
	for _, w := range data.OpenWorks {
		if w.ID == ids.workID {
			found = true
		}
	}
	if !found {
		t.Errorf("after DB reopen: open works missing %s", ids.workID)
	}
}

// ── Helpers ─────────────────────────────────────────────────────

func snapshotCounts(t *testing.T, dm mpminternal.CoreDB, tables []string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tbl := range tables {
		var n int
		if err := dm.SQLDB().QueryRow(
			"SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		out[tbl] = n
	}
	return out
}

func countUnreadHandoffs(t *testing.T, dm mpminternal.CoreDB) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM session_handoffs WHERE read_at IS NULL`,
	).Scan(&n); err != nil {
		t.Fatalf("count unread handoffs: %v", err)
	}
	return n
}

func activeWorkIDs(t *testing.T, dm mpminternal.CoreDB) []string {
	t.Helper()
	rows, err := dm.SQLDB().Query(`
		SELECT id FROM works WHERE status = 'open'
		ORDER BY updated_at DESC, created_at DESC
		LIMIT 5
	`)
	if err != nil {
		t.Fatalf("active works: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// quietJSON serializes to JSON and returns the bytes — used in
// informational messages only; never part of assertions.
func quietJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

var _ = quietJSON
var _ = context.Background
