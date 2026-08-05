package capability

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	internal "github.com/flowbyte-com/mpm-core"
)

// =============================================================================
// store_test.go — integration tests for the capability Store
//
// Strategy: stand up an in-memory SQLite via NewDatabaseManagerForDB,
// run dm.InitSchema() to materialize all MPM tables (including the 4
// capability tables added in F-1), wrap in a Store with a frozen
// clock, then exercise each public method.
//
// The frozen clock makes every audit field deterministic so the
// rollback cascade test can assert "this event was at T=42, that one
// at T=43" without time.Now drift. The clock advances on demand via
// helpers like advanceClock().
// =============================================================================

var capabilityTestDBCounter int64

// newTestStore returns a Store backed by an isolated in-memory
// SQLite with all MPM tables initialized and a frozen clock at T=0.
// The test owns the returned *sql.DB (callers should Close it on
// cleanup if they construct one themselves).
func newTestStore(t *testing.T) (*Store, *sql.DB, *frozenClock) {
	t.Helper()

	n := atomic.AddInt64(&capabilityTestDBCounter, 1)
	dsn := fmt.Sprintf("file:capability-test-%d?mode=memory&cache=shared&_fk=1", n)

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Enable foreign keys — required for the capability FKs to
	// enforce (cascades on dependency rows, NULL-on-delete for
	// author_theory_id, etc.).
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable FK: %v", err)
	}

	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	clk := &frozenClock{}
	clk.store(1700000000)
	store := NewStoreWithClock(dm, clk.fn)
	return store, db, clk
}

// frozenClock is the test seam for Store.Now(). Tests advance it via
// advance(n); reads return the current value.
type frozenClock struct {
	mu int64 // accessed via sync/atomic; no struct literal field
}

func (c *frozenClock) load() int64 {
	return atomic.LoadInt64(&c.mu)
}

func (c *frozenClock) store(v int64) {
	atomic.StoreInt64(&c.mu, v)
}

func (c *frozenClock) fn() time.Time {
	return time.Unix(c.load(), 0).UTC()
}

func (c *frozenClock) advance(seconds int64) {
	c.store(c.load() + seconds)
}

func (c *frozenClock) now() int64 {
	return c.load()
}

// =============================================================================
// helpers for building capability rows directly (bypassing the forge
// when a test needs to set state without going through draft)
// =============================================================================

// seedCapability inserts a capability row directly via the manager.
// Returns the id. Used to set up non-draft starting states for
// transition tests. Always inserts author_agent='' (empty string, not
// NULL) so scanCapability's `*string` reads don't trip.
//
// Hardcodes source_hash='deadbeef' which won't match any real
// source. Tests that need to invoke through Executor.Invoke (and
// thus trigger the source_hash check) should use
// seedCapabilityWithHash instead.
func seedCapability(t *testing.T, db *sql.DB, id, name string, state CapabilityState, createdFromID *string) {
	t.Helper()
	q := `INSERT INTO capabilities
	      (id, name, purpose, source_code, source_language, source_hash,
	       state, execution_domain, state_changed_at,
	       author_agent, created_from_id,
	       probation_required_success_count, probation_max_failure_rate,
	       tags, created_at, updated_at, metadata)
	      VALUES (?, ?, 'test', 'echo', 'bash', 'deadbeef',
	              ?, 'sandbox', ?,
	              '', ?,
	              5, 0.10,
	              '[]', ?, ?, '{}')`
	if _, err := db.Exec(q, id, name, state, 1700000000,
		createdFromID, 1700000000, 1700000000); err != nil {
		t.Fatalf("seedCapability: %v", err)
	}
}

// seedCapabilityWithHash inserts a capability row with caller-
// supplied source_code, source_language, source_hash, and metadata
// JSON. Used by EX-1/EX-2 tests that invoke through Executor.Invoke
// and need the stored hash to match the SHA-256 of the source
// passed to Invoke (otherwise the source_hash check refuses the
// invocation before telemetry is written).
//
// metadataJSON is written verbatim into the metadata column; pass
// "{}" for an empty metadata, or a JSON object for operator
// approval / limit overrides.
func seedCapabilityWithHash(t *testing.T, db *sql.DB, id, name string, state CapabilityState,
	sourceCode, sourceLanguage, sourceHash, metadataJSON string) {
	t.Helper()
	if metadataJSON == "" {
		metadataJSON = "{}"
	}
	q := `INSERT INTO capabilities
	      (id, name, purpose, source_code, source_language, source_hash,
	       state, execution_domain, state_changed_at,
	       author_agent, created_from_id,
	       probation_required_success_count, probation_max_failure_rate,
	       tags, created_at, updated_at, metadata)
	      VALUES (?, ?, 'test', ?, ?, ?,
	              ?, 'sandbox', ?,
	              '', NULL,
	              5, 0.10,
	              '[]', ?, ?, ?)`
	if _, err := db.Exec(q, id, name, sourceCode, sourceLanguage, sourceHash,
		state, 1700000000, 1700000000, 1700000000, metadataJSON); err != nil {
		t.Fatalf("seedCapabilityWithHash: %v", err)
	}
}

// recordInvocation is the telemetry insert the Executor would do in
// production. Bypasses the Store's RecordInvocationOutcome so tests
// can fast-forward metrics.
func recordInvocation(t *testing.T, db *sql.DB, id string, success bool) {
	t.Helper()
	exitCode := 0
	if !success {
		exitCode = 1
	}
	q := `INSERT INTO capability_invocations
	      (id, capability_id, invoked_at, exit_code, duration_ms, cascade_invalidated)
	      VALUES ('inv-` + id + `-` + fmt.Sprint(recordInvocationSeq()) + `',
	              ?, ?, ?, 100, 0)`
	if _, err := db.Exec(q, id, 1700000000, exitCode); err != nil {
		t.Fatalf("recordInvocation: %v", err)
	}
}

var invocationSeq int64

func recordInvocationSeq() int64 {
	return atomic.AddInt64(&invocationSeq, 1)
}

// setCounters sets success/failure counts directly on a capability
// row — used to fast-forward probation criteria.
func setCounters(t *testing.T, db *sql.DB, id string, success, failure int) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE capabilities SET success_count = ?, failure_count = ? WHERE id = ?`,
		success, failure, id,
	); err != nil {
		t.Fatalf("setCounters: %v", err)
	}
}

// =============================================================================
// tests
// =============================================================================

// TestStore_InsertCapabilityProposal_HappyPath verifies the canonical
// forge flow: validate → id assignment → row + dependency rows.
func TestStore_InsertCapabilityProposal_HappyPath(t *testing.T) {
	store, _, _ := newTestStore(t)

	id, err := store.InsertCapabilityProposal(&Proposal{
		Name:           "git-status",
		Purpose:        "report the working tree status",
		SourceCode:     "git status --porcelain",
		SourceLanguage: "bash",
		RequestedDomain: DomainSandbox,
		Tags:           StringSlice{"git", "vcs"},
	})
	if err != nil {
		t.Fatalf("InsertCapabilityProposal: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty id")
	}

	// Round-trip the row.
	c, err := store.GetCapability(id)
	if err != nil {
		t.Fatalf("GetCapability: %v", err)
	}
	if c.State != StateDraft {
		t.Errorf("new capability state = %s, want draft", c.State)
	}
	if c.Name != "git-status" {
		t.Errorf("name = %q, want git-status", c.Name)
	}
	if len(c.Tags) != 2 || c.Tags[0] != "git" {
		t.Errorf("tags round-trip lost: got %v", c.Tags)
	}
	if c.SourceHash == "" || c.SourceHash == "deadbeef" {
		t.Errorf("source_hash should be sha256, got %q", c.SourceHash)
	}
}

// TestStore_InsertCapabilityProposal_NameCollision verifies that a
// second proposal with the same name is rejected with ErrAlreadyExists.
func TestStore_InsertCapabilityProposal_NameCollision(t *testing.T) {
	store, _, _ := newTestStore(t)

	first := &Proposal{
		Name: "git-status", Purpose: "x", SourceCode: "echo hi",
		RequestedDomain: DomainSandbox,
	}
	if _, err := store.InsertCapabilityProposal(first); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	second := &Proposal{
		Name: "git-status", Purpose: "y", SourceCode: "echo bye",
		RequestedDomain: DomainSandbox,
	}
	_, err := store.InsertCapabilityProposal(second)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second insert err = %v, want ErrAlreadyExists", err)
	}
}

// TestStore_InsertCapabilityProposal_DependencyDead verifies that
// referencing a non-active capability as a dependency is refused.
func TestStore_InsertCapabilityProposal_DependencyDead(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Seed a draft capability — not active.
	depID := "dep-draft-1"
	seedCapability(t, db, depID, "draft-tool", StateDraft, nil)

	_, err := store.InsertCapabilityProposal(&Proposal{
		Name: "consumer", Purpose: "uses draft tool",
		SourceCode:        "echo consumer",
		RequestedDomain:   DomainSandbox,
		DependsOn:         []string{depID},
	})
	if !errors.Is(err, ErrDependencyDead) {
		t.Fatalf("err = %v, want ErrDependencyDead", err)
	}
	if !strings.Contains(err.Error(), depID) {
		t.Errorf("error should name the dead dependency: %v", err)
	}
}

// TestStore_InsertCapabilityProposal_DomainPolicyViolation verifies
// that drafts cannot request a domain above sandbox.
func TestStore_InsertCapabilityProposal_DomainPolicyViolation(t *testing.T) {
	store, _, _ := newTestStore(t)

	_, err := store.InsertCapabilityProposal(&Proposal{
		Name:           "sneaky",
		Purpose:        "ask for trusted",
		SourceCode:     "echo trusted",
		RequestedDomain: DomainTrusted,
	})
	if !errors.Is(err, ErrDomainPolicyViolation) {
		t.Fatalf("err = %v, want ErrDomainPolicyViolation", err)
	}
}

// TestStore_InsertCapabilityProposal_SourceTooLarge verifies the
// fast-fail size cap runs before any DB work.
func TestStore_InsertCapabilityProposal_SourceTooLarge(t *testing.T) {
	store, _, _ := newTestStore(t)

	huge := strings.Repeat("x", MaxSourceBytes+1)
	_, err := store.InsertCapabilityProposal(&Proposal{
		Name: "big", Purpose: "too much", SourceCode: huge,
		RequestedDomain: DomainSandbox,
	})
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("err = %v, want ErrSourceTooLarge", err)
	}
}

// TestStore_PromoteToActive_ProbationCriteria verifies that a
// probation capability with insufficient successes is rejected with
// a populated PromotionDecision.
func TestStore_PromoteToActive_ProbationCriteria(t *testing.T) {
	store, db, _ := newTestStore(t)

	id := "cap-prob-1"
	seedCapability(t, db, id, "prob-tool", StateProbation, nil)
	setCounters(t, db, id, 2, 0) // 2 of 5 required successes

	decision, err := store.PromoteToActive(id)
	if err == nil {
		t.Fatalf("expected criteria rejection, got nil")
	}
	if decision.Eligible {
		t.Errorf("decision.Eligible = true, want false")
	}
	if decision.RequiredSuccesses != 5 {
		t.Errorf("RequiredSuccesses = %d, want 5", decision.RequiredSuccesses)
	}
}

// TestStore_PromoteToActive_HappyPath verifies the full promotion:
// state transition, predecessor retirement, promotion event.
func TestStore_PromoteToActive_HappyPath(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Seed predecessor (active) and successor (probation).
	predecessor := "cap-prev"
	successor := "cap-next"
	seedCapability(t, db, predecessor, "prev-tool", StateActive, nil)
	seedCapability(t, db, successor, "next-tool", StateProbation, &predecessor)
	setCounters(t, db, successor, 10, 0) // plenty of successes

	// Promote successor.
	if _, err := store.PromoteToActive(successor); err != nil {
		t.Fatalf("PromoteToActive: %v", err)
	}

	// Successor should be active with promoted_at stamped.
	succ, err := store.GetCapability(successor)
	if err != nil {
		t.Fatalf("GetCapability(successor): %v", err)
	}
	if succ.State != StateActive {
		t.Errorf("successor state = %s, want active", succ.State)
	}
	if succ.PromotedAt == nil {
		t.Error("successor promoted_at not set")
	}

	// Predecessor should be retired with superseded_by_id pointing at successor.
	pred, err := store.GetCapability(predecessor)
	if err != nil {
		t.Fatalf("GetCapability(predecessor): %v", err)
	}
	if pred.State != StateRetired {
		t.Errorf("predecessor state = %s, want retired", pred.State)
	}
	if pred.SupersededByID == nil || *pred.SupersededByID != successor {
		t.Errorf("predecessor superseded_by_id = %v, want %s", pred.SupersededByID, successor)
	}

	// Two events should have been written: promotion + retirement.
	events, err := store.GetEvents(successor, 0, 100)
	if err != nil {
		t.Fatalf("GetEvents(successor): %v", err)
	}
	if len(events) < 1 {
		t.Errorf("expected promotion event, got %d", len(events))
	}
	predEvents, err := store.GetEvents(predecessor, 0, 100)
	if err != nil {
		t.Fatalf("GetEvents(predecessor): %v", err)
	}
	if len(predEvents) < 1 {
		t.Errorf("expected retirement event on predecessor, got %d", len(predEvents))
	}
}

// TestStore_ExecuteRollbackWithShatter_LineageCycle verifies the CTE's
// cycle guard: a corrupt created_from_id chain does not infinite-loop.
func TestStore_ExecuteRollbackWithShatter_LineageCycle(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Create a cycle: A → B → A (created_from_id chain).
	// Insert both rows first WITHOUT the cycle pointers (to satisfy
	// the FK), then UPDATE to create the loop.
	a, b := "cap-a", "cap-b"
	seedCapability(t, db, a, "tool-a", StateRetired, nil)
	seedCapability(t, db, b, "tool-b", StateActive, nil)
	if _, err := db.Exec(
		`UPDATE capabilities SET created_from_id = ? WHERE id = ?`, b, a,
	); err != nil {
		t.Fatalf("set A.created_from_id = B: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE capabilities SET created_from_id = ? WHERE id = ?`, a, b,
	); err != nil {
		t.Fatalf("set B.created_from_id = A: %v", err)
	}

	// findPredecessor must NOT infinite-loop. The depth cap + path guard
	// guarantee this; we run with a short timeout via test deadline.
	done := make(chan struct{})
	var predErr error
	go func() {
		_, predErr = store.findPredecessor(b)
		close(done)
	}()
	select {
	case <-done:
		// OK — completed without infinite-loop. With cycle, the path
		// guard skips revisits, so the walker finds no retired ancestor
		// reachable without revisit and returns ErrNotFound.
		if predErr != nil && !errors.Is(predErr, ErrNotFound) {
			t.Errorf("findPredecessor err = %v", predErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("findPredecessor did not return within 2s — cycle guard FAILED")
	}
}

// TestStore_ExecuteRollbackWithShatter_HappyPath verifies the full
// §5.2 cascade: predecessor revival + downstream shatter + events.
func TestStore_ExecuteRollbackWithShatter_HappyPath(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Build the graph:
	//   predecessor (retired) ← predecessor_id chain ← revision (active)
	//   downstream-dep-1 (active) depends on revision
	//   downstream-dep-2 (active) depends on revision
	predecessor := "cap-prev-rb"
	revision := "cap-rev-rb"
	downA := "cap-down-a"
	downB := "cap-down-b"
	seedCapability(t, db, predecessor, "prev-rb", StateRetired, nil)
	seedCapability(t, db, revision, "rev-rb", StateActive, &predecessor)
	seedCapability(t, db, downA, "down-a", StateActive, nil)
	seedCapability(t, db, downB, "down-b", StateActive, nil)
	if _, err := db.Exec(
		`INSERT INTO capability_dependencies (capability_id, depends_on_id, added_at)
		 VALUES (?, ?, 1700000000), (?, ?, 1700000000)`,
		downA, revision, downB, revision,
	); err != nil {
		t.Fatalf("seed deps: %v", err)
	}

	shattered, err := store.ExecuteRollbackWithShatter(revision, "tolerance_breach", "success_rate_delta")
	if err != nil {
		t.Fatalf("ExecuteRollbackWithShatter: %v", err)
	}
	if shattered != 2 {
		t.Errorf("shattered count = %d, want 2", shattered)
	}

	// Revision: rolled_back.
	rev, _ := store.GetCapability(revision)
	if rev.State != StateRolledBack {
		t.Errorf("revision state = %s, want rolled_back", rev.State)
	}

	// Predecessor: revived to active.
	pred, _ := store.GetCapability(predecessor)
	if pred.State != StateActive {
		t.Errorf("predecessor state = %s, want active (revived)", pred.State)
	}

	// Downstream: needs_revision with shatter_path in event metadata.
	for _, dc := range []string{downA, downB} {
		c, _ := store.GetCapability(dc)
		if c.State != StateNeedsRevision {
			t.Errorf("downstream %s state = %s, want needs_revision", dc, c.State)
		}
	}
	events, err := store.GetEvents(downA, 0, 10)
	if err != nil {
		t.Fatalf("GetEvents(downA): %v", err)
	}
	var foundPath bool
	for _, ev := range events {
		if ev.EventType == EventDependencyShatter {
			if p, ok := ev.Metadata["shatter_path"]; ok {
				foundPath = true
				if !strings.Contains(p.(string), downA) {
					t.Errorf("shatter_path %v should contain %s", p, downA)
				}
			}
		}
	}
	if !foundPath {
		t.Errorf("dependency_shatter event missing shatter_path metadata")
	}
}

// TestStore_ExecuteRollbackWithShatter_BothBrokenEscalation verifies
// the §5.2.1 escalation path: predecessor not in retired state
// causes the main tx to roll back, then a separate tx marks the
// revision as escalated.
func TestStore_ExecuteRollbackWithShatter_BothBrokenEscalation(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Predecessor is FRACTURED, not retired. The cascade cannot
	// revive it; the revision must escalate.
	predecessor := "cap-prev-fract"
	revision := "cap-rev-esc"
	seedCapability(t, db, predecessor, "prev-fract", StateFractured, nil)
	seedCapability(t, db, revision, "rev-esc", StateActive, &predecessor)

	_, err := store.ExecuteRollbackWithShatter(revision, "predecessor_unavailable", "success_rate_delta")
	if !errors.Is(err, ErrBothBroken) {
		t.Fatalf("err = %v, want ErrBothBroken", err)
	}

	// Revision is rolled_back (the escalation tx demoted it).
	rev, _ := store.GetCapability(revision)
	if rev.State != StateRolledBack {
		t.Errorf("revision state = %s, want rolled_back", rev.State)
	}

	// Metadata.escalated is set.
	if v, ok := rev.Metadata["escalated"]; !ok || v != true {
		t.Errorf("revision metadata.escalated = %v, want true", v)
	}

	// Predecessor is unchanged (still fractured).
	pred, _ := store.GetCapability(predecessor)
	if pred.State != StateFractured {
		t.Errorf("predecessor state = %s, want fractured (unchanged)", pred.State)
	}
}

// TestStore_ExecuteRollbackWithShatter_DiamondDependency verifies
// that the downstream CTE does not produce duplicate shatter events
// when two paths converge on the same node.
func TestStore_ExecuteRollbackWithShatter_DiamondDependency(t *testing.T) {
	store, db, _ := newTestStore(t)

	// Diamond:  revision
	//           /    \
	//         X        Y
	//           \    /
	//           leaf  ← appears via two paths, must shatter exactly once.
	revision := "cap-diamond-rev"
	x, y := "cap-diamond-x", "cap-diamond-y"
	leaf := "cap-diamond-leaf"
	seedCapability(t, db, revision, "rev-d", StateActive, nil)
	seedCapability(t, db, x, "x-d", StateActive, nil)
	seedCapability(t, db, y, "y-d", StateActive, nil)
	seedCapability(t, db, leaf, "leaf-d", StateActive, nil)

	for _, dep := range []struct{ from, to string }{ {x, revision}, {y, revision}, {leaf, x}, {leaf, y} } {
		if _, err := db.Exec(
			`INSERT INTO capability_dependencies (capability_id, depends_on_id, added_at) VALUES (?, ?, 1700000000)`,
			dep.from, dep.to,
		); err != nil {
			t.Fatalf("seed dep %s→%s: %v", dep.from, dep.to, err)
		}
	}

	shattered, err := store.ExecuteRollbackWithShatter(revision, "diamond test", "tolerance")
	if err != nil {
		t.Fatalf("ExecuteRollbackWithShatter: %v", err)
	}
	// Expect 3 distinct shatter targets (x, y, leaf), not 4.
	if shattered != 3 {
		t.Errorf("shattered count = %d, want 3 (cycle guard should dedupe)", shattered)
	}

	// Exactly one dependency_shatter event for leaf.
	events, _ := store.GetEvents(leaf, 0, 100)
	var shatterCount int
	for _, ev := range events {
		if ev.EventType == EventDependencyShatter {
			shatterCount++
		}
	}
	if shatterCount != 1 {
		t.Errorf("leaf shatter event count = %d, want 1", shatterCount)
	}
}
