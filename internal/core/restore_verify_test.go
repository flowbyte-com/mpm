package internal

import (
	"strings"
	"testing"
)

// managedInsert renders a test INSERT with an explicit column list.
func managedInsert(t *testing.T, table string, cols []string, vals ...string) string {
	t.Helper()
	if len(cols) != len(vals) {
		t.Fatalf("cols/vals mismatch for %s", table)
	}
	var b strings.Builder
	b.WriteString("INSERT INTO \"" + table + "\" (")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("\"" + c + "\"")
	}
	b.WriteString(") VALUES (")
	b.WriteString(strings.Join(vals, ", "))
	b.WriteString(")")
	return b.String()
}

func TestRestoreClassification(t *testing.T) {
	// Aware: supersession table + sentinel.
	aware := []string{
		`CREATE TABLE "agent_task_supersessions" ("id" TEXT)`,
		`CREATE TABLE "agent_tasks" ("id" TEXT)`,
		`INSERT INTO "schema_migrations" ("id") VALUES ('agent_task_supersessions_v1')`,
	}
	class, _, err := classifyRestoreDump(aware)
	if err != nil || class != restoreClassAware {
		t.Fatalf("aware classified as %v (err %v)", class, err)
	}
	// Incomplete: table without sentinel.
	incomplete := []string{
		`CREATE TABLE "agent_task_supersessions" ("id" TEXT)`,
		`CREATE TABLE "agent_tasks" ("id" TEXT)`,
		`INSERT INTO "schema_migrations" ("id") VALUES ('something_else')`,
	}
	if _, _, err := classifyRestoreDump(incomplete); err == nil ||
		!strings.Contains(err.Error(), "incomplete_supersession_schema") {
		t.Fatalf("incomplete dump returned %v", err)
	}
	// Unrecognised: assignment tables, no sentinel.
	unrec := []string{
		`CREATE TABLE "agent_tasks" ("id" TEXT)`,
	}
	if _, _, err := classifyRestoreDump(unrec); err == nil ||
		!strings.Contains(err.Error(), "unrecognised_assignment_schema") {
		t.Fatalf("unrecognised dump returned %v", err)
	}
	// PR3: sessions + sentinel, no supersession markers.
	pr3 := []string{
		`CREATE TABLE "agent_task_sessions" ("id" TEXT)`,
		`CREATE TABLE "agent_tasks" ("id" TEXT)`,
		`INSERT INTO "schema_migrations" ("id") VALUES ('agent_task_sessions_v1')`,
	}
	class, _, err = classifyRestoreDump(pr3)
	if err != nil || class != restoreClassPR3 {
		t.Fatalf("pr3 classified as %v (err %v)", class, err)
	}
	// Ordinary: no assignment tables.
	ord := []string{`CREATE TABLE "memories" ("id" TEXT)`}
	class, _, err = classifyRestoreDump(ord)
	if err != nil || class != restoreClassOrdinary {
		t.Fatalf("ordinary classified as %v (err %v)", class, err)
	}
}

func TestRestoreTriggerVerification(t *testing.T) {
	inv, err := canonicalManagedTriggers()
	if err != nil {
		t.Fatalf("inventory: %v", err)
	}
	// The live schema's own triggers verify clean.
	dm := NewTestLocalOnlyDM(t)
	rows, err := dm.SQLDB().Query(`SELECT name, sql FROM sqlite_master WHERE type='trigger'`)
	if err != nil {
		t.Fatal(err)
	}
	liveInv := &restoreDumpInventory{
		tables: map[string]bool{"agent_tasks": true, "agent_task_events": true,
			"agent_task_sessions": true, "agent_task_supersessions": true},
		sentinels: map[string]bool{restoreSentinelSupersessions: true},
		triggers:  map[string]string{}, triggerCounts: map[string]int{},
	}
	for rows.Next() {
		var name, sql string
		if err := rows.Scan(&name, &sql); err != nil {
			t.Fatal(err)
		}
		liveInv.triggers[name] = normalizeTriggerBody(sql)
		liveInv.triggerCounts[name]++
	}
	rows.Close()
	if _, err := verifyManagedTriggers(restoreClassAware, liveInv, inv); err != nil {
		t.Fatalf("live triggers rejected: %v", err)
	}
	// A modified body is refused.
	modInv := &restoreDumpInventory{
		tables: liveInv.tables, sentinels: liveInv.sentinels,
		triggers: map[string]string{}, triggerCounts: map[string]int{},
	}
	for name, body := range liveInv.triggers {
		modInv.triggers[name] = body
		modInv.triggerCounts[name] = 1
	}
	modInv.triggers["trg_agent_tasks_claim_epoch_guard"] =
		normalizeTriggerBody(`CREATE TRIGGER trg_agent_tasks_claim_epoch_guard
			BEFORE UPDATE OF claim_epoch ON agent_tasks FOR EACH ROW
			WHEN NEW.state = 'cancelled' BEGIN SELECT RAISE(ABORT, 'weakened'); END`)
	if _, err := verifyManagedTriggers(restoreClassAware, modInv, inv); err == nil ||
		!strings.Contains(err.Error(), "modified_canonical_trigger") {
		t.Fatalf("modified trigger returned %v", err)
	}
	// A missing trigger is refused.
	missInv := &restoreDumpInventory{
		tables: liveInv.tables, sentinels: liveInv.sentinels,
		triggers: map[string]string{}, triggerCounts: map[string]int{},
	}
	for name, body := range liveInv.triggers {
		if name == "trg_agent_tasks_claim_epoch_guard" {
			continue
		}
		missInv.triggers[name] = body
		missInv.triggerCounts[name] = 1
	}
	if _, err := verifyManagedTriggers(restoreClassAware, missInv, inv); err == nil ||
		!strings.Contains(err.Error(), "missing_canonical_trigger") {
		t.Fatalf("missing trigger returned %v", err)
	}
	// A duplicate definition is refused.
	dupInv := &restoreDumpInventory{
		tables: liveInv.tables, sentinels: liveInv.sentinels,
		triggers: map[string]string{}, triggerCounts: map[string]int{},
	}
	for name, body := range liveInv.triggers {
		dupInv.triggers[name] = body
		dupInv.triggerCounts[name] = 1
	}
	dupInv.triggerCounts["trg_agent_tasks_claim_epoch_guard"] = 2
	if _, err := verifyManagedTriggers(restoreClassAware, dupInv, inv); err == nil ||
		!strings.Contains(err.Error(), "duplicate_trigger_definition") {
		t.Fatalf("duplicate trigger returned %v", err)
	}
	// A recovery trigger in a pre-feature dump is unexpected.
	preInv := &restoreDumpInventory{
		tables:    map[string]bool{"agent_tasks": true},
		sentinels: map[string]bool{restoreSentinelAssignments: true},
		triggers:  map[string]string{}, triggerCounts: map[string]int{},
	}
	for name, body := range liveInv.triggers {
		preInv.triggers[name] = body
		preInv.triggerCounts[name] = 1
	}
	if _, err := verifyManagedTriggers(restoreClassPR1, preInv, inv); err == nil ||
		!strings.Contains(err.Error(), "unexpected_trigger") {
		t.Fatalf("future trigger in pre-feature dump returned %v", err)
	}
}

var taskCols = []string{"id", "project_id", "root_id", "parent_id", "depth", "state", "role", "claim_epoch"}
var sessionCols = []string{"id", "task_id", "attempt", "claim_epoch", "launch_state", "runtime_outcome", "created_at", "updated_at"}
var eventCols = []string{"id", "task_id", "event_index", "event_type", "claim_epoch", "created_at"}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func TestRestoreCompletedWithoutSuccessIsT106(t *testing.T) {
	stmts := []string{
		managedInsert(t, "agent_tasks", taskCols,
			q("at-1"), q("cpj-1"), q("at-1"), "NULL", "1", q("in_progress"), q("implementer"), "1"),
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-1"), q("at-1"), "1", "1", q("completed"), q("failed"), "100", "100"),
	}
	m, err := parseRestoreModel(stmts)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := validateRestoreHistory(m, restoreClassAware); err == nil ||
		!strings.Contains(err.Error(), "completed_without_success") {
		t.Fatalf("completed+failed returned %v", err)
	}
}

func TestRestoreAmbiguityConflictIsT107(t *testing.T) {
	stmts := []string{
		managedInsert(t, "agent_tasks", taskCols,
			q("at-1"), q("cpj-1"), q("at-1"), "NULL", "1", q("in_progress"), q("implementer"), "1"),
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-1"), q("at-1"), "1", "1", q("ambiguous"), "NULL", "100", "200"),
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-2"), q("at-1"), "2", "1", q("ambiguous"), "NULL", "110", "210"),
	}
	m, err := parseRestoreModel(stmts)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := validateRestoreHistory(m, restoreClassAware); err == nil ||
		!strings.Contains(err.Error(), "ambiguity_conflict") {
		t.Fatalf("double ambiguity returned %v", err)
	}
}

func TestRestoreReservedKeyWithoutLineage(t *testing.T) {
	stmts := []string{
		managedInsert(t, "agent_tasks", append(taskCols, "idempotency_key"),
			q("at-1"), q("cpj-1"), q("at-1"), "NULL", "1", q("enqueued"), q("implementer"), "0", q("supersede:ats-9")),
	}
	m, err := parseRestoreModel(stmts)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := validateRestoreHistory(m, restoreClassAware); err == nil ||
		!strings.Contains(err.Error(), "ErrReservedKeyCollision") {
		t.Fatalf("squatting row returned %v", err)
	}
}

// temporalFixture builds a single tree: root + implementer, claimed at
// epoch 1 (event at claimTime), one attempt.
func temporalFixture(t *testing.T, extra ...string) []string {
	t.Helper()
	base := []string{
		managedInsert(t, "agent_tasks", taskCols,
			q("at-root"), q("cpj-1"), q("at-root"), "NULL", "0", q("enqueued"), q("implementer"), "0"),
		managedInsert(t, "agent_tasks", taskCols,
			q("at-1"), q("cpj-1"), q("at-root"), q("at-root"), "1", q("in_progress"), q("implementer"), "1"),
		managedInsert(t, "agent_task_events", eventCols,
			q("ate-1"), q("at-1"), "0", q("claimed"), "1", "90"),
	}
	return append(base, extra...)
}

func TestRestorePostAmbiguityUnscopedIsT105(t *testing.T) {
	// Ambiguity recorded at updated_at=200; the unscoped sibling attempt
	// was created at 250 with a pre-ambiguity claim (90). The attempt
	// row postdates the recorded ambiguity: refused.
	stmts := temporalFixture(t,
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-amb"), q("at-1"), "1", "1", q("ambiguous"), "NULL", "150", "200"),
		managedInsert(t, "agent_tasks", taskCols,
			q("at-2"), q("cpj-1"), q("at-root"), q("at-root"), "1", q("in_progress"), q("implementer"), "1"),
		managedInsert(t, "agent_task_events", eventCols,
			q("ate-2"), q("at-2"), "0", q("claimed"), "1", "95"),
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-late"), q("at-2"), "1", "1", q("completed"), q("succeeded"), "250", "260"),
	)
	m, err := parseRestoreModel(stmts)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := validateRestoreHistory(m, restoreClassAware); err == nil ||
		!strings.Contains(err.Error(), "post_ambiguity_unscoped_session") {
		t.Fatalf("late unscoped attempt returned %v", err)
	}
}

func TestRestorePreAmbiguitySiblingAcceptedIsT108(t *testing.T) {
	// Same shape, but the sibling attempt (140) and its claim (95) both
	// strictly predate the recorded ambiguity (200): accepted.
	stmts := temporalFixture(t,
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-amb"), q("at-1"), "1", "1", q("ambiguous"), "NULL", "150", "200"),
		managedInsert(t, "agent_tasks", taskCols,
			q("at-2"), q("cpj-1"), q("at-root"), q("at-root"), "1", q("in_progress"), q("implementer"), "1"),
		managedInsert(t, "agent_task_events", eventCols,
			q("ate-2"), q("at-2"), "0", q("claimed"), "1", "95"),
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-early"), q("at-2"), "1", "1", q("completed"), q("succeeded"), "140", "150"),
	)
	m, err := parseRestoreModel(stmts)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := validateRestoreHistory(m, restoreClassAware); err != nil {
		t.Fatalf("early sibling refused: %v", err)
	}
}

func TestRestoreIndeterminateOrdering(t *testing.T) {
	// Claim event at the same time as the ambiguity: equal where
	// strictness is required.
	stmts := temporalFixture(t,
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-amb"), q("at-1"), "1", "1", q("ambiguous"), "NULL", "150", "200"),
		managedInsert(t, "agent_tasks", taskCols,
			q("at-2"), q("cpj-1"), q("at-root"), q("at-root"), "1", q("in_progress"), q("implementer"), "1"),
		managedInsert(t, "agent_task_events", eventCols,
			q("ate-2"), q("at-2"), "0", q("claimed"), "1", "200"),
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-eq"), q("at-2"), "1", "1", q("completed"), q("succeeded"), "150", "160"),
	)
	m, err := parseRestoreModel(stmts)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := validateRestoreHistory(m, restoreClassAware); err == nil ||
		!strings.Contains(err.Error(), "indeterminate_temporal_order") {
		t.Fatalf("equal timestamps returned %v", err)
	}
}

var lineageCols = []string{"id", "project_id", "root_id", "superseded_task_id",
	"replacement_task_id", "reviewer_task_id", "attempt_id",
	"authorised_by", "authority_kind", "evidence_ref", "justification", "created_at"}

// scopedTree builds: root + A (superseded at L1.created=100 by R/V,
// attempt X ambiguous) + optional L2 superseding R at 250.
func scopedTree(t *testing.T, withL2 bool, extra ...string) []string {
	t.Helper()
	base := []string{
		managedInsert(t, "agent_tasks", taskCols,
			q("at-root"), q("cpj-1"), q("at-root"), "NULL", "0", q("enqueued"), q("implementer"), "0"),
		managedInsert(t, "agent_tasks", taskCols,
			q("at-A"), q("cpj-1"), q("at-root"), q("at-root"), "1", q("in_progress"), q("implementer"), "1"),
		managedInsert(t, "agent_task_events", eventCols,
			q("ate-A"), q("at-A"), "0", q("claimed"), "1", "50"),
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-X"), q("at-A"), "1", "1", q("ambiguous"), "NULL", "80", "90"),
		managedInsert(t, "agent_tasks", taskCols,
			q("at-R"), q("cpj-1"), q("at-root"), q("at-root"), "1", q("enqueued"), q("implementer"), "0"),
		managedInsert(t, "agent_tasks", taskCols,
			q("at-V"), q("cpj-1"), q("at-root"), q("at-root"), "1", q("enqueued"), q("reviewer"), "0"),
		managedInsert(t, "agent_task_supersessions", lineageCols,
			q("atsup-1"), q("cpj-1"), q("at-root"), q("at-A"), q("at-R"), q("at-V"),
			q("ats-X"), q("op"), q("operator"), q("log:x"), q("why"), "100"),
	}
	if withL2 {
		base = append(base,
			managedInsert(t, "agent_task_sessions", sessionCols,
				q("ats-Y"), q("at-R"), "1", "1", q("ambiguous"), "NULL", "200", "210"),
			managedInsert(t, "agent_tasks", taskCols,
				q("at-R2"), q("cpj-1"), q("at-root"), q("at-root"), "1", q("enqueued"), q("implementer"), "0"),
			managedInsert(t, "agent_tasks", taskCols,
				q("at-V2"), q("cpj-1"), q("at-root"), q("at-root"), "1", q("enqueued"), q("reviewer"), "0"),
			managedInsert(t, "agent_task_supersessions", lineageCols,
				q("atsup-2"), q("cpj-1"), q("at-root"), q("at-R"), q("at-R2"), q("at-V2"),
				q("ats-Y"), q("op"), q("operator"), q("log:y"), q("why2"), "250"),
		)
	}
	return append(base, extra...)
}

func mustValidate(t *testing.T, stmts []string) error {
	t.Helper()
	m, err := parseRestoreModel(stmts)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return validateRestoreHistory(m, restoreClassAware)
}

func TestRestoreScopedAttemptInBoundsIsT129(t *testing.T) {
	stmts := scopedTree(t, false,
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-ok"), q("at-R"), "1", "1", q("completed"), q("succeeded"), "150", "160"),
	)
	if err := mustValidate(t, stmts); err != nil {
		t.Fatalf("in-bounds scoped attempt refused: %v", err)
	}
}

func TestRestoreAttemptBeforeAuthorisationIsT126(t *testing.T) {
	stmts := scopedTree(t, false,
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-early"), q("at-R"), "1", "1", q("completed"), q("succeeded"), "90", "95"),
	)
	if err := mustValidate(t, stmts); err == nil ||
		!strings.Contains(err.Error(), "attempt_before_authorisation") {
		t.Fatalf("pre-lineage attempt returned %v", err)
	}
}

func TestRestorePostSupersessionActivityIsT123(t *testing.T) {
	stmts := scopedTree(t, true,
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-late"), q("at-R"), "2", "1", q("completed"), q("succeeded"), "300", "310"),
	)
	if err := mustValidate(t, stmts); err == nil ||
		!strings.Contains(err.Error(), "post_supersession_activity") {
		t.Fatalf("post-supersession replacement activity returned %v", err)
	}
}

func TestRestoreObsoleteReviewerActivityIsT124(t *testing.T) {
	stmts := scopedTree(t, true,
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-obs"), q("at-V"), "1", "1", q("completed"), q("succeeded"), "300", "310"),
	)
	if err := mustValidate(t, stmts); err == nil ||
		!strings.Contains(err.Error(), "obsolete_reviewer_activity") {
		t.Fatalf("obsolete reviewer activity returned %v", err)
	}
}

func TestRestoreHistoricalDescendantActivityIsT125(t *testing.T) {
	stmts := scopedTree(t, true,
		managedInsert(t, "agent_tasks", taskCols,
			q("at-D"), q("cpj-1"), q("at-root"), q("at-R"), "2", q("in_progress"), q("implementer"), "1"),
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-desc"), q("at-D"), "1", "1", q("completed"), q("succeeded"), "300", "310"),
	)
	if err := mustValidate(t, stmts); err == nil ||
		!strings.Contains(err.Error(), "historical_descendant_activity") {
		t.Fatalf("descendant activity returned %v", err)
	}
}

func TestRestorePostSupersessionOnOriginalIsC2(t *testing.T) {
	// A non-ambiguous attempt on the superseded original recorded after
	// the naming lineage: the task's authorisation ended with
	// supersession.
	stmts := scopedTree(t, false,
		managedInsert(t, "agent_task_sessions", sessionCols,
			q("ats-after"), q("at-A"), "2", "1", q("completed"), q("succeeded"), "150", "160"),
	)
	if err := mustValidate(t, stmts); err == nil ||
		!strings.Contains(err.Error(), "post_supersession_activity") {
		t.Fatalf("C2 activity returned %v", err)
	}
}
