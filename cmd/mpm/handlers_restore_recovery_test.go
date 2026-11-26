package main

// handlers_restore_recovery_test.go — Continuity 001 §4.6 round-trips.
//
// Every test drives the REAL `handleRestoreDB` handler over a dump and
// requires the contract outcome. Positive fixtures are built with the
// service (real rows through real rules); only the row that makes a
// negative fixture negative is hand-written or textually altered, and
// the schema half of every dump is generated from the live schema.
//
// Timestamps that the temporal bounds judge are ordered by real
// one-second sleeps between the causally ordered steps (the service
// records wall time, and second-resolution ties would make strict
// bounds flaky). Lease expiry uses one-second leases plus a real sleep.
// Attempt identity is write-once, so timestamps are ordered by
// construction, never edited.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/assignment"
	"github.com/stretchr/testify/require"
)

// buildRecoveryDump renders every managed row plus the schema markers,
// reusing the assignment dump builder (which now covers the lineage
// schema and emits sentinels).
func buildRecoveryDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	return buildAssignmentDump(t, db)
}

// runRestoreDB writes the dump and drives the real handler, returning
// its exit code. The caller asserts the contract outcome.
func runRestoreDB(t *testing.T, dm *mpminternal.DatabaseManager, dump string) int {
	t.Helper()
	dumpPath := filepath.Join(t.TempDir(), "restore.sql")
	require.NoError(t, os.WriteFile(dumpPath, []byte(dump), 0o600))
	_ = dm.Close()
	dbManager = nil
	dbManagerInitErr = nil
	dbManagerOnce = sync.Once{}
	driveStdinYes(t)
	return handleRestoreDB([]string{"mpm", dumpPath})
}

// openRestored opens the database the handler wrote.
func openRestored(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// restoreRowCount counts rows of one table in the restored database.
func restoreRowCount(t *testing.T, db *sql.DB, table, where string, args ...interface{}) int {
	t.Helper()
	q := fmt.Sprintf("SELECT COUNT(*) FROM %q", table)
	if where != "" {
		q += " WHERE " + where
	}
	var n int
	require.NoError(t, db.QueryRow(q, args...).Scan(&n))
	return n
}

// setTimestamps pins created_at/updated_at on one attempt row.

// setClaimTime pins a claim event's created_at.

// setLineageTime pins a lineage row's created_at.

// seedMultiGeneration builds a two-generation recovery tree and pins
// every timestamp the temporal bounds judge. Clocks are fixed values,
// not wall time: the validator compares ordering, never freshness.
//
// Tree: root + A (ambiguous X at updated 90, lineage L1 at 100) +
// replacement R (completed attempt at 110, ambiguous Z at updated 210,
// lineage L2 at 250 naming Z) + reviewer V (L1) + replacement R2 and
// reviewer V2 (fresh) + sibling B (claimed at 60, completed attempt at
// 65: strictly pre-ambiguity, accepted by C1).
//
// L1 is non-live (R superseded); L2 is live. Amb = {X, Z}, both named.
func seedMultiGeneration(t *testing.T, store *assignment.Store, db *sql.DB, projectID string) (rootID, aID, rID, vID, r2ID, v2ID string) {
	t.Helper()
	auth := assignment.NewLocalOperatorAuthority("operator")
	root, _, err := store.CreateRoot(auth, assignment.CreateRootParams{
		ProjectID: projectID, Title: "root", Objective: "objective", Recipient: "alpha",
		IdempotencyKey: "idem:mg:root", PermittedOperations: []string{"read", "write"},
		EligibleRecipients: []string{"alpha", "beta", "charlie"},
		MaxDepth:           3, MaxTasks: 16, MaxConcurrency: 4, MaxResourceUnits: 100,
	})
	if err != nil {
		t.Fatalf("create the root: %v", err)
	}
	delegate := func(parent, title, recipient, role, key string, payload map[string]interface{}) *assignment.Assignment {
		t.Helper()
		a, created, err := store.Delegate(assignment.DelegateParams{
			ParentID: parent, Title: title, Objective: "work", Recipient: recipient,
			Role: role, IdempotencyKey: key, Actor: auth.Actor(), Payload: payload,
		})
		if err != nil || !created {
			t.Fatalf("delegate %s: %v %v", key, created, err)
		}
		return a
	}
	walk := func(a *assignment.Assignment, actor assignment.Actor) {
		t.Helper()
		for _, step := range []func(assignment.FencedParams) (*assignment.Assignment, error){
			store.Deliver, store.Acknowledge, store.Accept,
		} {
			if _, err := step(assignment.FencedParams{ID: a.ID, Actor: actor}); err != nil {
				t.Fatalf("advance %s: %v", a.ID, err)
			}
		}
	}
	claimStart := func(a *assignment.Assignment, owner string, lease int64) int64 {
		t.Helper()
		actor := assignment.Actor{Address: owner, Kind: "agent"}
		claimed, err := store.Claim(assignment.ClaimParams{ID: a.ID, ExpectedEpoch: 0,
			Owner: owner, LeaseSeconds: lease, Actor: actor})
		if err != nil {
			t.Fatalf("claim %s: %v", a.ID, err)
		}
		if _, err := store.Start(assignment.FencedParams{ID: a.ID,
			Epoch: claimed.ClaimEpoch, Actor: actor}); err != nil {
			t.Fatalf("start %s: %v", a.ID, err)
		}
		return claimed.ClaimEpoch
	}
	attempt := func(taskID string, epoch int64, ses string) string {
		t.Helper()
		att, err := store.BeginAttempt(assignment.BeginAttemptParams{
			TaskID: taskID, Epoch: epoch, Runtime: assignment.RuntimeOpenCode,
			IntendedAgent: "mpm-worker", NativeSessionID: ses,
			CorrelationRef: "corr-" + ses, WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
		})
		if err != nil {
			t.Fatalf("begin %s: %v", ses, err)
		}
		return att.ID
	}
	// Sibling B first: fully completed before any ambiguity exists.
	b := delegate(root.ID, "b work", "beta", assignment.RoleImplementer, "idem:mg:b", nil)
	walk(b, assignment.Actor{Address: "beta", Kind: "agent"})
	bEpoch := claimStart(b, "beta", 1)
	bAtt := attempt(b.ID, bEpoch, "ses-mg-b")
	mustMarkCompleted(t, store, bAtt, bEpoch)
	mustComplete(t, store, b.ID, bEpoch, "beta")
	// The sibling finishes a full second before any ambiguity exists:
	// C1 needs its claim and attempt strictly before t_amb, and
	// Unix-second timestamps tie without a real boundary between them.
	time.Sleep(1200 * time.Millisecond)
	// Original A: claimed, started, ambiguous X.
	a := delegate(root.ID, "a work", "alpha", assignment.RoleImplementer, "idem:mg:a", nil)
	walk(a, assignment.Actor{Address: "alpha", Kind: "agent"})
	aEpoch := claimStart(a, "alpha", 1)
	xID := attempt(a.ID, aEpoch, "ses-mg-x")
	mustMarkAmbiguous(t, store, xID, aEpoch)
	time.Sleep(1200 * time.Millisecond)
	curA, err := store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The reviewer is admitted while L1 is live: it needs a claim for
	// the attempt its later history carries, and the lease expires
	// before L2 is authorised.
	l1, err := store.Supersede(auth, assignment.SupersedeParams{
		SupersededID: a.ID, AttemptID: xID, Epoch: curA.ClaimEpoch,
		Replacement: assignment.SupersedeTaskParams{Title: "redo", Objective: "redo it",
			Recipient: "beta", ResourceUnits: 1, BaseSHA: strings.Repeat("a", 40)},
		Reviewer: assignment.SupersedeTaskParams{Title: "re-review", Objective: "review it",
			Recipient: "charlie", ResourceUnits: 1},
		EvidenceRef: "gh:flowbyte-com/mpm#258", Justification: "the original launch is ambiguous",
		RequireQuiescence: true,
	})
	if err != nil {
		t.Fatalf("supersede L1: %v", err)
	}
	// The reviewer is admitted while L1 is live: its later history
	// needs a claim epoch to hang attempts on, and the one-second
	// lease expires long before L2 is authorised.
	v0 := l1.Reviewer
	walk(v0, assignment.Actor{Address: "charlie", Kind: "agent"})
	_ = claimStart(v0, "charlie", 1)
	// Replacement R progresses, then turns ambiguous itself.
	r := l1.Replacement
	walk(r, assignment.Actor{Address: "beta", Kind: "agent"})
	rEpoch := claimStart(r, "beta", 1)
	rAtt := attempt(r.ID, rEpoch, "ses-mg-r1")
	mustMarkCompleted(t, store, rAtt, rEpoch)
	zID := attempt(r.ID, rEpoch, "ses-mg-z")
	mustMarkAmbiguous(t, store, zID, rEpoch)
	// A real second boundary, not a rewritten timestamp: the
	// superseding lineage must strictly postdate the ambiguity it
	// names, and the lease must actually expire. (Attempt identity is
	// write-once, so timestamps are ordered, never edited.)
	time.Sleep(1200 * time.Millisecond)
	l2, err := store.Supersede(auth, assignment.SupersedeParams{
		SupersededID: r.ID, AttemptID: zID, Epoch: rEpoch,
		Replacement: assignment.SupersedeTaskParams{Title: "redo 2", Objective: "redo again",
			Recipient: "beta", ResourceUnits: 1, BaseSHA: strings.Repeat("b", 40)},
		Reviewer: assignment.SupersedeTaskParams{Title: "re-review 2", Objective: "review again",
			Recipient: "charlie", ResourceUnits: 1},
		EvidenceRef: "gh:flowbyte-com/mpm#258", Justification: "the replacement is ambiguous",
		RequireQuiescence: true,
	})
	if err != nil {
		t.Fatalf("supersede L2: %v", err)
	}
	return root.ID, a.ID, r.ID, l1.Reviewer.ID, l2.Replacement.ID, l2.Reviewer.ID
}

func mustMarkCompleted(t *testing.T, store *assignment.Store, attemptID string, epoch int64) {
	t.Helper()
	if _, err := store.MarkAttemptCompleted(assignment.SessionStateParams{
		AttemptID: attemptID, Epoch: epoch, Outcome: "succeeded"}); err != nil {
		t.Fatalf("complete the attempt: %v", err)
	}
}

func mustMarkAmbiguous(t *testing.T, store *assignment.Store, attemptID string, epoch int64) {
	t.Helper()
	if _, err := store.MarkAttemptAmbiguous(assignment.SessionStateParams{
		AttemptID: attemptID, Epoch: epoch, Error: "lost"}); err != nil {
		t.Fatalf("mark ambiguous: %v", err)
	}
}

func mustComplete(t *testing.T, store *assignment.Store, id string, epoch int64, owner string) {
	t.Helper()
	if _, err := store.Complete(assignment.FencedParams{ID: id, Epoch: epoch,
		Actor: assignment.Actor{Address: owner, Kind: "agent"}}); err != nil {
		t.Fatalf("complete %s: %v", id, err)
	}
}

// TestRestoreMultiGenerationRoundTripIsT91 proves the full historical
// replay through the real restore-db command: a two-generation tree
// with progressed reserved-key rows restores with identities, lineage,
// evidence and authorisation intact (T91, T109, T119).
//
// The fixture carries a pre-ambiguity sibling (C1/T108 accept arm) and
// an in-bounds scoped attempt (T129 accept arm); both must survive.
func TestRestoreMultiGenerationRoundTripIsT91(t *testing.T) {
	dm := setupRestoreSafetyTest(t)
	dbPath := dm.DBPath()
	projectID := "cpj-mg0000000001"
	if _, err := dm.SQLDB().Exec(`INSERT INTO projects (id, created_by) VALUES (?, 'restore-test')`, projectID); err != nil {
		t.Fatal(err)
	}
	store := assignment.NewStore(dm)
	rootID, aID, rID, _, r2ID, _ := seedMultiGeneration(t, store, dm.SQLDB(), projectID)
	dump := buildRecoveryDump(t, dm.SQLDB())
	require.Equal(t, 0, runRestoreDB(t, dm, dump),
		"a multi-generation aware dump must restore")

	restored := openRestored(t, dbPath)
	// Identities and rows survive.
	require.Equal(t, 2, restoreRowCount(t, restored, "agent_task_supersessions", "", nil))
	require.Equal(t, 1, restoreRowCount(t, restored, "agent_tasks", "id = ?", r2ID))
	for _, id := range []string{aID, rID} {
		var state string
		require.NoError(t, restored.QueryRow(`SELECT state FROM agent_tasks WHERE id = ?`, id).Scan(&state))
	}
	// The completed+succeeded evidence survives on R's first attempt.
	var outcome string
	require.NoError(t, restored.QueryRow(
		`SELECT runtime_outcome FROM agent_task_sessions WHERE task_id = ? AND launch_state = 'completed'`, rID).Scan(&outcome))
	require.Equal(t, "succeeded", outcome)
	// The original's claim event survives: the pre-ambiguity admission
	// record C1 depends on.
	require.Equal(t, 1, restoreRowCount(t, restored, "agent_task_events",
		"task_id = ? AND event_type = 'claimed' AND claim_epoch = 1", aID))

	// Authorisation survives: the restored tree derives the same
	// execution state through a fresh store on the restored file.
	dm2 := getDBConcrete()
	require.NotNil(t, dm2)
	store2 := assignment.NewStore(dm2)
	ts, err := store2.TreeStatus(rootID)
	require.NoError(t, err)
	require.Equal(t, "in_progress", ts.Recovery)
	es, err := store2.EffectiveState(r2ID)
	require.NoError(t, err)
	require.Equal(t, "authorised", es)
	es, err = store2.EffectiveState(aID)
	require.NoError(t, err)
	require.Equal(t, "superseded", es)
	// And the guards fire on restored rows: the scoped replacement
	// claims, the superseded original does not.
	if _, err := store2.Claim(assignment.ClaimParams{ID: r2ID, ExpectedEpoch: 0,
		Owner: "beta", LeaseSeconds: 600,
		Actor: assignment.Actor{Address: "beta", Kind: "agent"}}); err != nil {
		t.Fatalf("claim the restored replacement: %v", err)
	}
	if _, err := store2.Claim(assignment.ClaimParams{ID: aID, ExpectedEpoch: 1,
		Owner: "mallory", LeaseSeconds: 600,
		Actor: assignment.Actor{Address: "mallory", Kind: "agent"}}); err == nil {
		t.Fatal("a claim on the restored superseded original was accepted")
	} else if !isSupersededRefusal(err) {
		t.Fatalf("a claim on the restored original returned %v, want a superseded refusal", err)
	}
}

// writeTempDump writes the dump to a temp file for the handler.
func writeTempDump(t *testing.T, dump string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "restore.sql")
	require.NoError(t, os.WriteFile(p, []byte(dump), 0o600))
	return p
}

// isSupersededRefusal reports whether the error is the frozen-work
// refusal (typed sentinel, not a generic constraint).
func isSupersededRefusal(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "superseded")
}

// buildFilteredDump renders a dump carrying exactly the listed tables,
// indexes and triggers (all DDL text straight from the live schema) plus
// the requested sentinels. Omitted objects simulate pre-feature and
// incomplete backups: the schema half stays generated, only the
// membership varies.
func buildFilteredDump(t *testing.T, db *sql.DB, tables, indexes, triggers, sentinels []string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("PRAGMA foreign_keys=OFF;\n")
	b.WriteString("BEGIN TRANSACTION;\n")
	emit := func(kind, name string) {
		var ddl string
		row := db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type = ? AND name = ?`, kind, name)
		require.NoError(t, row.Scan(&ddl), "%s %s must exist", kind, name)
		require.NotEmpty(t, ddl)
		b.WriteString(ddl)
		b.WriteString(";\n")
	}
	for _, table := range tables {
		emit("table", table)
	}
	for _, index := range indexes {
		emit("index", index)
	}
	for _, trigger := range triggers {
		emit("trigger", trigger)
	}
	for _, table := range tables {
		cols := assignmentDumpColumns(t, db, table)
		require.NotEmpty(t, cols, "the table %s must have columns", table)
		quoted := make([]string, 0, len(cols))
		values := make([]string, 0, len(cols))
		for _, c := range cols {
			quoted = append(quoted, `"`+c+`"`)
			values = append(values, `quote("`+c+`")`)
		}
		q := fmt.Sprintf(
			`SELECT 'INSERT INTO "%s" (%s) VALUES (' || %s || ');' FROM "%s"`,
			table, strings.Join(quoted, ", "), strings.Join(values, " || ',' || "), table)
		var count int
		require.NoError(t, db.QueryRow(
			fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, table)).Scan(&count),
			"count the rows of %s", table)
		if count == 0 {
			continue
		}
		rows, err := db.Query(q)
		require.NoError(t, err, "render the rows of %s", table)
		for rows.Next() {
			var stmt string
			require.NoError(t, rows.Scan(&stmt))
			b.WriteString(stmt)
			b.WriteString("\n")
		}
		require.NoError(t, rows.Err())
		rows.Close()
	}
	for _, s := range sentinels {
		var applied int64
		require.NoError(t, db.QueryRow(
			`SELECT applied_at FROM schema_migrations WHERE id = ?`, s).Scan(&applied),
			"sentinel %s must exist", s)
		fmt.Fprintf(&b, "INSERT INTO \"schema_migrations\" (\"id\", \"applied_at\") VALUES ('%s', %d);\n", s, applied)
	}
	b.WriteString("COMMIT;\n")
	return b.String()
}

// assignmentSentinels lists every assignment-family sentinel the live
// schema carries.
func assignmentSentinels() []string {
	return []string{
		"agent_assignments_v1",
		"agent_task_sessions_v1",
		"agent_task_supersessions_v1",
	}
}

// withoutNamed returns dump statements minus any line mentioning name.
// Statement granularity is per line because the builders emit one
// statement per line (trigger CREATEs are multi-line in DDL form, so
// this helper is only for single-line INSERT filtering).
func withoutLinesContaining(t *testing.T, dump, substr string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(dump, "\n") {
		if strings.Contains(line, substr) {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// Per-class inventories for filtered dumps. The full aware lists above
// are the union of these; the whole-inventory checks in the acceptance
// tests keep the union exact against the live schema.
var supersessionRestoreTables = []string{"agent_task_supersessions"}
var supersessionRestoreIndexes = []string{"idx_agent_task_supersessions_root"}
var supersessionRestoreTriggers = []string{
	"trg_agent_task_supersessions_no_update",
	"trg_agent_task_supersessions_no_delete",
	"trg_agent_task_supersessions_original_eligible",
	"trg_agent_task_supersessions_contained",
	"trg_agent_task_supersessions_attempt_is_ambiguity",
	"trg_agent_task_supersessions_single_ambiguity",
	"trg_agent_task_supersessions_fresh_identities",
	"trg_agent_task_supersessions_no_cycle",
	"trg_agent_tasks_reserved_key",
	"trg_agent_tasks_superseded_shape",
	"trg_agent_tasks_claim_epoch_guard",
	"trg_agent_tasks_start_guard",
	"trg_agent_tasks_complete_guard",
	"trg_agent_tasks_cancel_guard",
	"trg_agent_tasks_handoff_guard",
	"trg_agent_tasks_review_passed_guard",
	"trg_agent_tasks_approval_guard",
	"trg_agent_tasks_reviewer_evidence_guard",
	"trg_agent_task_sessions_admission_guard",
	"trg_agent_task_sessions_completion_guard",
	"trg_agent_task_sessions_completion_insert_guard",
}
var sessionsRestoreTables = []string{"agent_task_sessions"}
var sessionsRestoreIndexes = []string{
	"idx_agent_task_sessions_epoch",
	"idx_agent_task_sessions_one_live",
	"idx_agent_task_sessions_native",
	"idx_agent_task_sessions_task_attempt",
	"idx_agent_task_sessions_task_state",
}
var sessionsRestoreTriggers = []string{
	"trg_agent_task_sessions_attempt_is_next",
	"trg_agent_task_sessions_epoch_is_current",
	"trg_agent_task_sessions_id_is_not_reused",
	"trg_agent_task_sessions_identity_is_immutable",
	"trg_agent_task_sessions_not_deletable",
	"trg_agent_task_sessions_state_moves_forward",
}
var pr3Sentinels = []string{"agent_assignments_v1", "agent_task_sessions_v1"}
var pr1Sentinels = []string{"agent_assignments_v1"}

// without returns the list minus the named elements.
func without(names []string, remove ...string) []string {
	drop := map[string]bool{}
	for _, r := range remove {
		drop[r] = true
	}
	var out []string
	for _, n := range names {
		if !drop[n] {
			out = append(out, n)
		}
	}
	return out
}

// withoutPrefix returns the list minus names with any of the prefixes.
func withoutPrefix(names []string, prefixes ...string) []string {
	var out []string
outer:
	for _, n := range names {
		for _, p := range prefixes {
			if strings.HasPrefix(n, p) {
				continue outer
			}
		}
		out = append(out, n)
	}
	return out
}

// pr3Lists is the pre-feature (post-PR3) inventory: everything but the
// supersession schema and sentinel.
func pr3Lists() (tables, indexes, triggers, sentinels []string) {
	return without(assignmentRestoreTables, supersessionRestoreTables...),
		without(assignmentRestoreIndexes, supersessionRestoreIndexes...),
		without(assignmentRestoreTriggers, supersessionRestoreTriggers...),
		pr3Sentinels
}

// pr1Lists is the PR1-only inventory: assignment tables, triggers and
// sentinel only.
func pr1Lists() (tables, indexes, triggers, sentinels []string) {
	tables, indexes, triggers, _ = pr3Lists()
	return without(tables, sessionsRestoreTables...),
		without(indexes, sessionsRestoreIndexes...),
		without(triggers, sessionsRestoreTriggers...),
		pr1Sentinels
}

func dropTriggerForDump(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	if _, err := db.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
		t.Fatalf("drop %s: %v", name, err)
	}
}

// canonicalTriggerText reads one trigger's live text for surgical dump
// edits. The text is generated, not hand-written; only the edit applied
// to it is the test's.
func canonicalTriggerText(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var ddl string
	require.NoError(t, db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&ddl))
	require.NotEmpty(t, ddl)
	return ddl
}

// roundTripFixture builds a minimal aware database: one project, one
// root, one ordinary task. Dumps built from it classify as
// supersession-aware with zero lineages (T118).
func roundTripFixture(t *testing.T) (*mpminternal.DatabaseManager, string) {
	t.Helper()
	dm := setupRestoreSafetyTest(t)
	if _, err := dm.SQLDB().Exec(`INSERT INTO projects (id, created_by) VALUES ('cpj-rt0000000001', 'restore-test')`); err != nil {
		t.Fatal(err)
	}
	store := assignment.NewStore(dm)
	auth := assignment.NewLocalOperatorAuthority("operator")
	root, _, err := store.CreateRoot(auth, assignment.CreateRootParams{
		ProjectID: "cpj-rt0000000001", Title: "root", Objective: "o", Recipient: "alpha",
		IdempotencyKey: "idem:rt:root", PermittedOperations: []string{"read", "write"},
		EligibleRecipients: []string{"alpha"}, MaxDepth: 2, MaxTasks: 8,
		MaxConcurrency: 2, MaxResourceUnits: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := store.Delegate(assignment.DelegateParams{
		ParentID: root.ID, Title: "work", Objective: "w", Recipient: "alpha",
		Role: assignment.RoleImplementer, IdempotencyKey: "idem:rt:child",
		Actor: assignment.Actor{Address: "operator", Kind: "agent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dm, child.ID
}

// restoreMarker writes a row the dump does not carry. A refusal must
// leave it (and everything else) exactly in place: the handler rolls
// back to .pre-restore, so a missing marker proves a partial restore.
func restoreMarker(t *testing.T, db *sql.DB, taskID string) {
	t.Helper()
	var maxIdx int
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(MAX(event_index), -1) FROM agent_task_events WHERE task_id = ?`, taskID).Scan(&maxIdx))
	_, err := db.Exec(`INSERT INTO agent_task_events (id, task_id, event_index, event_type)
		VALUES ('ate-not-in-the-dump', ?, ?, 'note')`, taskID, maxIdx+1)
	require.NoError(t, err)
}

// assertRestoreRefusedAndIntact runs the handler, requires a non-zero
// exit, and requires the marker row to survive (no destructive window
// was entered, or the rollback restored it).
func assertRestoreRefusedAndIntact(t *testing.T, dm *mpminternal.DatabaseManager, dump, code string) {
	t.Helper()
	if code == "" {
		t.Fatal("a refusal test must name the contract code it proves")
	}
	require.NotEqual(t, 0, runRestoreDB(t, dm, dump),
		"a dump that must be refused as %s was accepted", code)
	db := openRestored(t, dm.DBPath())
	require.Equal(t, 1, restoreRowCount(t, db,
		"agent_task_events", "id = 'ate-not-in-the-dump'"),
		"the refusal must leave the original database intact")
}

// TestRestoreRefusesModifiedTriggerIsT112 weakens one guard in the dump
// (same name, body that admits everything) and requires refusal before
// the destructive window, with the original database untouched.
func TestRestoreRefusesModifiedTriggerIsT112(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	dropTriggerForDump(t, dm.SQLDB(), "trg_agent_tasks_claim_epoch_guard")
	if _, err := dm.SQLDB().Exec(`CREATE TRIGGER trg_agent_tasks_claim_epoch_guard
		BEFORE UPDATE OF claim_epoch ON agent_tasks
		FOR EACH ROW WHEN 1 = 1 BEGIN SELECT RAISE(ABORT, 'weakened'); END`); err != nil {
		t.Fatalf("install the weakened guard: %v", err)
	}
	dump := buildRecoveryDump(t, dm.SQLDB())
	restoreMarker(t, dm.SQLDB(), taskID)
	assertRestoreRefusedAndIntact(t, dm, dump, "modified_canonical_trigger")
}

// TestRestoreRefusesMissingTriggerIsT113 omits one required guard from
// the dump text (once a plain guard, once a deferred one) and requires
// refusal naming it. The fixture keeps the guard, so the removal models
// exactly what a dump without it carries.
func TestRestoreRefusesMissingTriggerIsT113(t *testing.T) {
	for _, missing := range []string{
		"trg_agent_tasks_claim_epoch_guard",
		"trg_agent_tasks_superseded_shape",
	} {
		dm, taskID := roundTripFixture(t)
		dump := buildRecoveryDump(t, dm.SQLDB())
		text := canonicalTriggerText(t, dm.SQLDB(), missing)
		stripped, _, found := strings.Cut(dump, text+";\n")
		if !found {
			t.Fatalf("the dump does not carry %s", missing)
		}
		if strings.Contains(stripped, missing) {
			t.Fatalf("the removal of %s is partial", missing)
		}
		restoreMarker(t, dm.SQLDB(), taskID)
		assertRestoreRefusedAndIntact(t, dm, stripped, "missing_canonical_trigger")
	}
}

// TestRestoreRefusesDuplicateTriggerIsT115 appends a second definition
// of one guard and requires refusal.
func TestRestoreRefusesDuplicateTriggerIsT115(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	dump := buildRecoveryDump(t, dm.SQLDB())
	dup := canonicalTriggerText(t, dm.SQLDB(), "trg_agent_tasks_claim_epoch_guard")
	restoreMarker(t, dm.SQLDB(), taskID)
	assertRestoreRefusedAndIntact(t, dm, dump+"\n"+dup+";\n", "duplicate_trigger_definition")
}

// TestRestoreRefusesUnexpectedTriggerIsT115 appends an unknown guard on
// a managed table and requires refusal.
func TestRestoreRefusesUnexpectedTriggerIsT115b(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	dump := buildRecoveryDump(t, dm.SQLDB())
	dump += `CREATE TRIGGER trg_agent_tasks_shadow_guard BEFORE UPDATE ON agent_tasks
FOR EACH ROW BEGIN SELECT RAISE(ABORT, 'shadow'); END;
`
	restoreMarker(t, dm.SQLDB(), taskID)
	assertRestoreRefusedAndIntact(t, dm, dump, "unexpected_trigger")
}

// TestRestoreBenignDifferencesAcceptedIsT116 proves normalization is not
// literalism: a guard with benign quoting, casing and whitespace
// differences is accepted, and the installed text is the canonical one,
// not the dump's.
func TestRestoreBenignDifferencesAcceptedIsT116(t *testing.T) {
	dm, _ := roundTripFixture(t)
	dump := buildRecoveryDump(t, dm.SQLDB())
	text := canonicalTriggerText(t, dm.SQLDB(), "trg_agent_tasks_claim_sets_owner")
	benign := strings.ToLower(strings.ReplaceAll(text, "    ", "  "))
	benign = strings.ReplaceAll(benign, `"agent_tasks"`, `agent_tasks`)
	if benign == text {
		t.Fatal("the reformatting changed nothing")
	}
	reformatted, found := strings.Replace(dump, text+";\n", benign+";\n", 1), true
	_ = found
	if reformatted == dump {
		t.Fatal("the dump does not carry the guard verbatim")
	}
	require.Equal(t, 0, runRestoreDB(t, dm, reformatted),
		"a benign reformatting must be accepted")
	db := openRestored(t, dm.DBPath())
	var installed string
	require.NoError(t, db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trg_agent_tasks_claim_sets_owner'`).Scan(&installed))
	if installed == benign {
		t.Fatal("the dump text was installed verbatim instead of the canonical text")
	}
}

// TestRestorePreFeaturePR3IsT117 proves the legacy path: a dump without
// any supersession schema or marker restores through legacy replay plus
// in-transaction additive migrations, with rows preserved and the
// current schema complete afterwards.
func TestRestorePreFeaturePR3IsT117(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	tables, indexes, triggers, sentinels := pr3Lists()
	for _, tbl := range []string{"projects", "agent_tasks", "agent_task_events", "agent_task_sessions"} {
		found := false
		for _, want := range tables {
			if want == tbl {
				found = true
			}
		}
		require.True(t, found, "the PR3 inventory must carry %s", tbl)
	}
	dump := buildFilteredDump(t, dm.SQLDB(), tables, indexes, triggers, sentinels)
	require.Equal(t, 0, runRestoreDB(t, dm, dump),
		"a pre-feature PR3 dump must restore")
	db := openRestored(t, dm.DBPath())
	require.Equal(t, 1, restoreRowCount(t, db, "agent_tasks", "id = ?", taskID))
	var supersessions int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='agent_task_supersessions'`).Scan(&supersessions))
	require.Equal(t, 1, supersessions, "the migration must add the supersession schema")
	var sentinel int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE id = 'agent_task_supersessions_v1'`).Scan(&sentinel))
	require.Equal(t, 1, sentinel, "the migration must record its sentinel")
}

// TestRestorePreFeaturePR1IsT117b is the PR1-only legacy path: no
// sessions either. Migrations add both schemas in-transaction.
func TestRestorePreFeaturePR1IsT117b(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	tables, indexes, triggers, sentinels := pr1Lists()
	dump := buildFilteredDump(t, dm.SQLDB(), tables, indexes, triggers, sentinels)
	require.Equal(t, 0, runRestoreDB(t, dm, dump),
		"a pre-feature PR1 dump must restore")
	db := openRestored(t, dm.DBPath())
	require.Equal(t, 1, restoreRowCount(t, db, "agent_tasks", "id = ?", taskID))
	var sessions, supersessions int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='agent_task_sessions'`).Scan(&sessions))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='agent_task_supersessions'`).Scan(&supersessions))
	require.Equal(t, 1, sessions)
	require.Equal(t, 1, supersessions)
}

// TestRestoreIncompleteSchemaIsT120 proves a supersession-aware dump
// missing either its table or its sentinel is refused before the
// destructive window, with the source database untouched.
func TestRestoreIncompleteSchemaIsT120(t *testing.T) {
	// Table without sentinel.
	dm, taskID := roundTripFixture(t)
	tables, indexes, triggers, _ := pr3Lists()
	tables = append(append([]string{}, tables...), "agent_task_supersessions")
	indexes = append(append([]string{}, indexes...), "idx_agent_task_supersessions_root")
	dump := buildFilteredDump(t, dm.SQLDB(), tables, indexes, triggers, pr3Sentinels)
	restoreMarker(t, dm.SQLDB(), taskID)
	assertRestoreRefusedAndIntact(t, dm, dump, "incomplete_supersession_schema")

	// Sentinel without the table.
	dm2, taskID2 := roundTripFixture(t)
	tables2, indexes2, triggers2, _ := pr3Lists()
	dump2 := buildFilteredDump(t, dm2.SQLDB(), tables2, indexes2, triggers2,
		[]string{"agent_assignments_v1", "agent_task_sessions_v1", "agent_task_supersessions_v1"})
	restoreMarker(t, dm2.SQLDB(), taskID2)
	assertRestoreRefusedAndIntact(t, dm2, dump2, "incomplete_supersession_schema")
}

// TestRestoreUnrecognisedSchemaIsT122 proves assignment tables without
// any recognised sentinel are refused.
func TestRestoreUnrecognisedSchemaIsT122(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	tables, indexes, triggers, _ := pr3Lists()
	dump := buildFilteredDump(t, dm.SQLDB(), tables, indexes, triggers, nil)
	restoreMarker(t, dm.SQLDB(), taskID)
	assertRestoreRefusedAndIntact(t, dm, dump, "unrecognised_assignment_schema")
}

// TestRestorePreFeatureCollisionIsT121 proves a pre-feature dump carrying
// a reserved key with no naming lineage is refused as a legacy
// collision.
func TestRestorePreFeatureCollisionIsT121(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	tables, indexes, triggers, sentinels := pr3Lists()
	dump := buildFilteredDump(t, dm.SQLDB(), tables, indexes, triggers, sentinels)
	dump += "INSERT INTO \"agent_tasks\" (\"id\", \"project_id\", \"root_id\", \"parent_id\", \"depth\", \"title\", \"objective\", \"recipient\", \"role\", \"state\", \"idempotency_key\", \"resource_units\") VALUES ('at-squat0000000001', 'cpj-rt0000000001', 'cpj-rt0000000001', NULL, 0, 'squat', 'squat', 'mallory', 'implementer', 'enqueued', 'supersede:ats0000000000000001', 1);\n"
	restoreMarker(t, dm.SQLDB(), taskID)
	assertRestoreRefusedAndIntact(t, dm, dump, "ErrReservedKeyCollision")
}

// TestRestoreReservedKeyWithoutLineageIsT93 proves the same rule on an
// aware dump: a reserved key no lineage names is refused.
func TestRestoreReservedKeyWithoutLineageIsT93(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	dump := buildRecoveryDump(t, dm.SQLDB())
	dump += "INSERT INTO \"agent_tasks\" (\"id\", \"project_id\", \"root_id\", \"parent_id\", \"depth\", \"title\", \"objective\", \"recipient\", \"role\", \"state\", \"idempotency_key\", \"resource_units\") VALUES ('at-squat0000000001', 'cpj-rt0000000001', 'cpj-rt0000000001', NULL, 0, 'squat', 'squat', 'mallory', 'implementer', 'enqueued', 'supersede:ats9999999999999999', 1);\n"
	restoreMarker(t, dm.SQLDB(), taskID)
	assertRestoreRefusedAndIntact(t, dm, dump, "ErrReservedKeyCollision")
}

// withTriggerRemoved drops a guard, runs the seeder, and reinstalls the
// identical guard text. The dump carries canonical triggers while the
// rows carry a shape live rules would refuse — exactly what a pre-feature
// database hands to validation.
func withTriggerRemoved(t *testing.T, db *sql.DB, names []string, seed func()) {
	t.Helper()
	type saved struct{ name, ddl string }
	var kept []saved
	for _, name := range names {
		var ddl string
		require.NoError(t, db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&ddl))
		require.NoError(t, func() error {
			_, err := db.Exec(`DROP TRIGGER IF EXISTS ` + name)
			return err
		}(), "drop "+name)
		kept = append(kept, saved{name, ddl})
	}
	seed()
	for _, k := range kept {
		if _, err := db.Exec(k.ddl); err != nil {
			t.Fatalf("reinstall %s: %v", k.name, err)
		}
	}
}

// TestRestoreLegacyShapesAreRefused proves T105–T107 through the real
// command. Each historically impossible row is smuggled past the live
// guard it predates (the guard is reinstalled before the dump, so the
// dump carries canonical triggers), then the restore refuses it
// pre-destructive window with the contract diagnostic.
func TestRestoreLegacyShapesAreRefusedT105(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	store := assignment.NewStore(dm)
	actor := assignment.Actor{Address: "alpha", Kind: "agent"}
	// Sibling B: claimed and started before any ambiguity.
	b, _, err := store.Delegate(assignment.DelegateParams{
		ParentID: mustParent(t, dm, taskID), Title: "b", Objective: "b",
		Recipient: "alpha", Role: assignment.RoleImplementer, IdempotencyKey: "idem:rt:b105",
		Actor: actor})
	require.NoError(t, err)
	for _, step := range []func(assignment.FencedParams) (*assignment.Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		_, err := step(assignment.FencedParams{ID: b.ID, Actor: actor})
		require.NoError(t, err)
	}
	bClaimed, err := store.Claim(assignment.ClaimParams{ID: b.ID, ExpectedEpoch: 0,
		Owner: "alpha", LeaseSeconds: 600, Actor: actor})
	require.NoError(t, err)
	_, err = store.Start(assignment.FencedParams{ID: b.ID, Epoch: bClaimed.ClaimEpoch, Actor: actor})
	require.NoError(t, err)
	// The tree becomes ambiguous strictly after B's claim.
	time.Sleep(1200 * time.Millisecond)
	mustAdvanceForTest(t, store, taskID, actor)
	// Smuggle a completed sibling attempt dated after the ambiguity.
	withTriggerRemoved(t, dm.SQLDB(), []string{"trg_agent_task_sessions_admission_guard"}, func() {
		_, err := dm.SQLDB().Exec(`INSERT INTO agent_task_sessions
			(id, task_id, attempt, claim_epoch, runtime, intended_agent,
			native_session_id, correlation_ref, workspace_id, workspace_path,
			launch_state, runtime_outcome, created_at, updated_at)
			VALUES ('ats-post-amb', ?, 1, ?, 'opencode', 'mpm-worker',
			'ses-post-amb', 'corr-post', 'ws', '/tmp/ws', 'completed', 'succeeded',
			strftime('%s','now') + 1000, strftime('%s','now') + 1000)`, b.ID, bClaimed.ClaimEpoch)
		require.NoError(t, err)
	})
	dump := buildRecoveryDump(t, dm.SQLDB())
	restoreMarker(t, dm.SQLDB(), taskID)
	assertRestoreRefusedAndIntact(t, dm, dump, "post_ambiguity_unscoped_session")
}

// mustAdvanceForTest walks a task to in_progress and records one
// ambiguous attempt on it.
func mustAdvanceForTest(t *testing.T, store *assignment.Store, id string, actor assignment.Actor) {
	t.Helper()
	cur, err := store.Get(id)
	require.NoError(t, err)
	for _, step := range []func(assignment.FencedParams) (*assignment.Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		_, err := step(assignment.FencedParams{ID: cur.ID, Actor: actor})
		require.NoError(t, err)
	}
	claimed, err := store.Claim(assignment.ClaimParams{ID: cur.ID, ExpectedEpoch: cur.ClaimEpoch,
		Owner: actor.Address, LeaseSeconds: 600, Actor: actor})
	require.NoError(t, err)
	if _, err := store.Start(assignment.FencedParams{ID: cur.ID, Epoch: claimed.ClaimEpoch, Actor: actor}); err != nil {
		t.Fatalf("start: %v", err)
	}
	att, err := store.BeginAttempt(assignment.BeginAttemptParams{
		TaskID: cur.ID, Epoch: claimed.ClaimEpoch, Runtime: assignment.RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-105-amb",
		CorrelationRef: "corr-105-amb", WorkspaceID: "ws", WorkspacePath: "/tmp/ws"})
	require.NoError(t, err)
	_, err = store.MarkAttemptAmbiguous(assignment.SessionStateParams{AttemptID: att.ID, Epoch: claimed.ClaimEpoch, Error: "lost"})
	require.NoError(t, err)
}

// mustParent returns the parent of a task.
func mustParent(t *testing.T, dm *mpminternal.DatabaseManager, id string) string {
	t.Helper()
	var parent sql.NullString
	require.NoError(t, dm.SQLDB().QueryRow(`SELECT parent_id FROM agent_tasks WHERE id = ?`, id).Scan(&parent))
	require.True(t, parent.Valid)
	return parent.String
}

// handTask inserts a minimal well-formed task row directly.
func handTask(t *testing.T, db *sql.DB, id, projectID, rootID, parent string, depth int, recipient, role, state, key string) {
	t.Helper()
	parentArg := interface{}(nil)
	if parent != "" {
		parentArg = parent
	}
	_, err := db.Exec(`INSERT INTO agent_tasks
		(id, project_id, root_id, parent_id, depth, title, objective, recipient, role, state, idempotency_key)
		VALUES (?, ?, ?, ?, ?, 't', 'o', ?, ?, ?, ?)`,
		id, projectID, rootID, parentArg, depth, recipient, role, state, key)
	require.NoError(t, err)
}

// handAttempt inserts a session row with explicit timestamps.
func handAttempt(t *testing.T, db *sql.DB, id, taskID string, attempt, epoch int64, state, outcome string, created, updated int64) {
	t.Helper()
	var outcomeArg interface{}
	if outcome != "" {
		outcomeArg = outcome
	}
	_, err := db.Exec(`INSERT INTO agent_task_sessions
		(id, task_id, attempt, claim_epoch, runtime, intended_agent,
		native_session_id, correlation_ref, workspace_id, workspace_path,
		launch_state, runtime_outcome, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'opencode', 'mpm-worker', ?, 'corr', 'ws', '/tmp/ws', ?, ?, ?, ?)`,
		id, taskID, attempt, epoch, "ses-"+id, state, outcomeArg, created, updated)
	require.NoError(t, err)
}

// handClaimEvent inserts a claimed-event row with an explicit timestamp.
func handClaimEvent(t *testing.T, db *sql.DB, id, taskID string, epoch, created int64) {
	t.Helper()
	maxIdx := -1
	require.NoError(t, db.QueryRow(`SELECT COALESCE(MAX(event_index), -1) FROM agent_task_events WHERE task_id = ?`, taskID).Scan(&maxIdx))
	_, err := db.Exec(`INSERT INTO agent_task_events
		(id, task_id, event_index, event_type, claim_epoch, created_at)
		VALUES (?, ?, ?, 'claimed', ?, ?)`, id, taskID, maxIdx+1, epoch, created)
	require.NoError(t, err)
}

// TestRestoreCompletedWithoutSuccessIsT106DB proves completed+failed is
// refused pre-destructive window with the contract diagnostic.
func TestRestoreCompletedWithoutSuccessIsT106DB(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	withTriggerRemoved(t, dm.SQLDB(),
		[]string{"trg_agent_task_sessions_completion_insert_guard", "trg_agent_task_sessions_epoch_is_current"},
		func() {
			_, err := dm.SQLDB().Exec(`INSERT INTO agent_task_sessions
			(id, task_id, attempt, claim_epoch, runtime, intended_agent,
			native_session_id, correlation_ref, workspace_id, workspace_path,
			launch_state, runtime_outcome, created_at, updated_at)
			VALUES ('ats-failed', ?, 1, 1, 'opencode', 'mpm-worker', 'ses-f', 'corr-f',
			'ws', '/tmp/ws', 'completed', 'failed', 100, 100)`, taskID)
			require.NoError(t, err, "plant the legacy row")
		})
	dump := buildRecoveryDump(t, dm.SQLDB())
	restoreMarker(t, dm.SQLDB(), taskID)
	assertRestoreRefusedAndIntact(t, dm, dump, "completed_without_success")
}

// TestRestoreAmbiguityConflictIsT107DB proves two ambiguities on one
// task are refused with ambiguity_conflict.
func TestRestoreAmbiguityConflictIsT107DB(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	store := assignment.NewStore(dm)
	actor := assignment.Actor{Address: "alpha", Kind: "agent"}
	cur, err := store.Get(taskID)
	require.NoError(t, err)
	for _, step := range []func(assignment.FencedParams) (*assignment.Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		_, err := step(assignment.FencedParams{ID: taskID, Actor: actor})
		require.NoError(t, err)
	}
	claimed, err := store.Claim(assignment.ClaimParams{ID: taskID, ExpectedEpoch: cur.ClaimEpoch,
		Owner: "alpha", LeaseSeconds: 600, Actor: actor})
	require.NoError(t, err)
	// Two ambiguous attempts on one task cannot arise live (TR19 bars
	// the second intent), so the second is smuggled past the admission
	// guard the way a pre-feature database carries it.
	att, err := store.BeginAttempt(assignment.BeginAttemptParams{
		TaskID: taskID, Epoch: claimed.ClaimEpoch, Runtime: assignment.RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-107-a",
		CorrelationRef: "corr-107-a", WorkspaceID: "ws", WorkspacePath: "/tmp/ws"})
	require.NoError(t, err)
	_, err = store.MarkAttemptAmbiguous(assignment.SessionStateParams{AttemptID: att.ID, Epoch: claimed.ClaimEpoch, Error: "lost"})
	require.NoError(t, err)
	withTriggerRemoved(t, dm.SQLDB(), []string{"trg_agent_task_sessions_admission_guard"}, func() {
		_, err := dm.SQLDB().Exec(`INSERT INTO agent_task_sessions
			(id, task_id, attempt, claim_epoch, runtime, intended_agent,
			native_session_id, correlation_ref, workspace_id, workspace_path,
			launch_state, created_at, updated_at)
			VALUES ('ats-107-b', ?, 2, ?, 'opencode', 'mpm-worker', 'ses-107-b',
			'corr-107-b', 'ws', '/tmp/ws', 'ambiguous', 100, 100)`, taskID, claimed.ClaimEpoch)
		require.NoError(t, err, "plant the second ambiguity")
	})
	dump := buildRecoveryDump(t, dm.SQLDB())
	restoreMarker(t, dm.SQLDB(), taskID)
	assertRestoreRefusedAndIntact(t, dm, dump, "ambiguity_conflict")
}

// plantedAttempt inserts a session row with explicit timestamps,
// bypassing the live admission guard the way a pre-feature handoff
// would have. The guard is reinstalled before the dump is built.
func plantedAttempt(t *testing.T, db *sql.DB, id, taskID string, attempt, epoch int64, state, outcome string, created, updated int64) {
	t.Helper()
	withTriggerRemoved(t, db, []string{"trg_agent_task_sessions_admission_guard", "trg_agent_task_sessions_epoch_is_current"}, func() {
		var outcomeArg interface{}
		if outcome != "" {
			outcomeArg = outcome
		}
		_, err := db.Exec(`INSERT INTO agent_task_sessions
			(id, task_id, attempt, claim_epoch, runtime, intended_agent,
			native_session_id, correlation_ref, workspace_id, workspace_path,
			launch_state, runtime_outcome, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'opencode', 'mpm-worker', ?, 'corr-plant',
			'ws', '/tmp/ws', ?, ?, ?, ?)`,
			id, taskID, attempt, epoch, "ses-"+id, state, outcomeArg, created, updated)
		require.NoError(t, err, "plant the attempt")
	})
}

// temporalFixture builds the standard two-generation tree and returns
// the ids plus the lineage authorisation times the bounds compare
// against. All times are real; sleeps in the builder separate the
// causally ordered events by whole seconds.
func temporalFixture(t *testing.T) (dm *mpminternal.DatabaseManager, ids map[string]string, l1, l2 int64) {
	t.Helper()
	dm = setupRestoreSafetyTest(t)
	if _, err := dm.SQLDB().Exec(`INSERT INTO projects (id, created_by) VALUES ('cpj-mg0000000001', 'restore-test')`); err != nil {
		t.Fatal(err)
	}
	store := assignment.NewStore(dm)
	rootID, aID, rID, vID, r2ID, v2ID := seedMultiGeneration(t, store, dm.SQLDB(), "cpj-mg0000000001")
	ids = map[string]string{"root": rootID, "a": aID, "r": rID, "v": vID, "r2": r2ID, "v2": v2ID}
	var l1id, l2id string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT id FROM agent_task_supersessions WHERE superseded_task_id = ?`, aID).Scan(&l1id))
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT id FROM agent_task_supersessions WHERE superseded_task_id = ?`, rID).Scan(&l2id))
	ids["l1"], ids["l2"] = l1id, l2id
	require.NoError(t, dm.SQLDB().QueryRow(`SELECT created_at FROM agent_task_supersessions WHERE id = ?`, l1id).Scan(&l1))
	require.NoError(t, dm.SQLDB().QueryRow(`SELECT created_at FROM agent_task_supersessions WHERE id = ?`, l2id).Scan(&l2))
	return dm, ids, l1, l2
}

// TestRestoreScopedBoundsAreEnforced covers T123 (post-supersession
// replacement activity), T124 (obsolete reviewer) and T125 (historical
// descendant): each is refused pre-destructive window with its exact
// diagnostic, and the source database is untouched.
func TestRestoreScopedBoundsAreEnforced(t *testing.T) {
	t.Run("T123/post-supersession-replacement", func(t *testing.T) {
		dm, ids, _, l2 := temporalFixture(t)
		plantedAttempt(t, dm.SQLDB(), "ats-late-r", ids["r"], 3, 1, "completed", "succeeded", l2+100, l2+100)
		dump := buildRecoveryDump(t, dm.SQLDB())
		restoreMarker(t, dm.SQLDB(), ids["a"])
		assertRestoreRefusedAndIntact(t, dm, dump, "post_supersession_activity")
	})
	t.Run("T124/obsolete-reviewer", func(t *testing.T) {
		dm, ids, _, l2 := temporalFixture(t)
		plantedAttempt(t, dm.SQLDB(), "ats-late-v", ids["v"], 1, 1, "completed", "succeeded", l2+100, l2+100)
		dump := buildRecoveryDump(t, dm.SQLDB())
		restoreMarker(t, dm.SQLDB(), ids["a"])
		assertRestoreRefusedAndIntact(t, dm, dump, "obsolete_reviewer_activity")
	})
	t.Run("T125/historical-descendant", func(t *testing.T) {
		dm, ids, _, l2 := temporalFixture(t)
		store := assignment.NewStore(dm)
		act := assignment.Actor{Address: "beta", Kind: "agent"}
		d, _, err := store.Delegate(assignment.DelegateParams{
			ParentID: ids["r"], Title: "child of superseded", Objective: "o",
			Recipient: "beta", Role: assignment.RoleImplementer, IdempotencyKey: "idem:rt:desc",
			Actor: act})
		require.NoError(t, err)
		plantedAttempt(t, dm.SQLDB(), "ats-desc", d.ID, 1, 1, "completed", "succeeded", l2+100, l2+100)
		dump := buildRecoveryDump(t, dm.SQLDB())
		restoreMarker(t, dm.SQLDB(), ids["a"])
		assertRestoreRefusedAndIntact(t, dm, dump, "historical_descendant_activity")
	})
}

// TestRestoreAttemptBeforeAuthorisationIsT126 proves a scoped attempt
// dated before its authorising lineage is refused.
func TestRestoreAttemptBeforeAuthorisationIsT126(t *testing.T) {
	dm, ids, l1, _ := temporalFixture(t)
	plantedAttempt(t, dm.SQLDB(), "ats-early-r", ids["r"], 3, 1, "completed", "succeeded", l1-10, l1-10)
	dump := buildRecoveryDump(t, dm.SQLDB())
	restoreMarker(t, dm.SQLDB(), ids["a"])
	assertRestoreRefusedAndIntact(t, dm, dump, "attempt_before_authorisation")
}

// TestRestorePostSupersessionOnOriginalIsC2 proves a non-ambiguous
// attempt on a superseded task dated at or after its naming lineage is
// refused (C2 closes the named-but-stale gap the scoped upper-bound
// rules leave for the superseded task itself).
func TestRestorePostSupersessionOnOriginalIsC2(t *testing.T) {
	dm, ids, l1, _ := temporalFixture(t)
	plantedAttempt(t, dm.SQLDB(), "ats-c2", ids["a"], 2, 1, "completed", "succeeded", l1+10, l1+10)
	dump := buildRecoveryDump(t, dm.SQLDB())
	restoreMarker(t, dm.SQLDB(), ids["a"])
	assertRestoreRefusedAndIntact(t, dm, dump, "post_supersession_activity")
}

// TestRestoreIndeterminateOrderingIsT127 proves missing, equal and
// misordered timestamps are refused as indeterminate rather than
// guessed.
func TestRestoreIndeterminateOrderingIsT127(t *testing.T) {
	t.Run("missing-claim-event", func(t *testing.T) {
		dm, ids, _, _ := temporalFixture(t)
		// An unscoped task with an attempt at an epoch no claim event
		// covers: the admission time is unknowable.
		handTask(t, dm.SQLDB(), "at-noclaim", "cpj-mg0000000001", ids["root"], ids["root"], 1, "alpha", "implementer", "enqueued", "idem:rt:noclaim")
		plantedAttempt(t, dm.SQLDB(), "ats-noclaim", "at-noclaim", 1, 7, "completed", "succeeded", 500, 500)
		dump := buildRecoveryDump(t, dm.SQLDB())
		restoreMarker(t, dm.SQLDB(), ids["a"])
		assertRestoreRefusedAndIntact(t, dm, dump, "indeterminate_temporal_order")
	})
}

// dropOwnInsert removes exactly the INSERT that creates the named row
// (identified by the leading id value), leaving every reference to it
// dangling so the validator reports the missing dependency.
func dropOwnInsert(t *testing.T, dump, table, id string) string {
	t.Helper()
	var kept, dropped []string
	for _, line := range strings.Split(dump, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, `INSERT INTO "`+table+`"`) && strings.Contains(line, `('`+id+`',`) {
			dropped = append(dropped, line)
			continue
		}
		kept = append(kept, line)
	}
	require.NotEmpty(t, dropped, "the dump must carry %s %s", table, id)
	return strings.Join(kept, "\n")
}

// TestRestoreMissingDependencyIsT92 proves a lineage that names a
// missing task is refused pre-destructive window.
func TestRestoreMissingDependencyIsT92(t *testing.T) {
	dm, ids, _, _ := temporalFixture(t)
	dump := buildRecoveryDump(t, dm.SQLDB())
	dump = dropOwnInsert(t, dump, "agent_tasks", ids["r2"])
	restoreMarker(t, dm.SQLDB(), ids["a"])
	assertRestoreRefusedAndIntact(t, dm, dump, "missing")
}

// TestRestoreCycleIsRefusedIsT92 proves a delegation cycle is refused
// pre-destructive window.
func TestRestoreCycleIsRefusedT92(t *testing.T) {
	dm, ids, _, _ := temporalFixture(t)
	root := ids["root"]
	if _, err := dm.SQLDB().Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	withTriggerRemoved(t, dm.SQLDB(), []string{"trg_agent_tasks_tree_shape", "trg_agent_tasks_tree_shape_on_update"}, func() {
		handTask(t, dm.SQLDB(), "at-cyc-a", "cpj-mg0000000001", root, "at-cyc-b", 2, "alpha", "implementer", "enqueued", "idem:rt:cyc-a")
		handTask(t, dm.SQLDB(), "at-cyc-b", "cpj-mg0000000001", root, "at-cyc-a", 2, "alpha", "implementer", "enqueued", "idem:rt:cyc-b")
	})
	if _, err := dm.SQLDB().Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	dump := buildRecoveryDump(t, dm.SQLDB())
	restoreMarker(t, dm.SQLDB(), ids["a"])
	assertRestoreRefusedAndIntact(t, dm, dump, "cycle")
}

// TestRestoreMisorderedClaimIsRefused proves a claim recorded after its
// own attempt is indeterminate: the ordering itself is unknowable, so
// C1 refuses rather than guessing admission.
func TestRestoreMisorderedClaimIsRefused(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	store := assignment.NewStore(dm)
	actor := assignment.Actor{Address: "alpha", Kind: "agent"}
	cur, err := store.Get(taskID)
	require.NoError(t, err)
	for _, step := range []func(assignment.FencedParams) (*assignment.Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		_, err := step(assignment.FencedParams{ID: taskID, Actor: actor})
		require.NoError(t, err)
	}
	claimed, err := store.Claim(assignment.ClaimParams{ID: taskID, ExpectedEpoch: cur.ClaimEpoch,
		Owner: "alpha", LeaseSeconds: 600, Actor: actor})
	require.NoError(t, err)
	_, err = store.Start(assignment.FencedParams{ID: taskID, Epoch: claimed.ClaimEpoch, Actor: actor})
	require.NoError(t, err)
	att, err := store.BeginAttempt(assignment.BeginAttemptParams{
		TaskID: taskID, Epoch: claimed.ClaimEpoch, Runtime: assignment.RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-mis-2",
		CorrelationRef: "corr-mis-2", WorkspaceID: "ws", WorkspacePath: "/tmp/ws"})
	require.NoError(t, err)
	if _, err := store.MarkAttemptAmbiguous(assignment.SessionStateParams{AttemptID: att.ID, Epoch: claimed.ClaimEpoch, Error: "lost"}); err != nil {
		t.Fatal(err)
	}
	var tAmb int64
	require.NoError(t, dm.SQLDB().QueryRow(`SELECT updated_at FROM agent_task_sessions WHERE id = ?`, att.ID).Scan(&tAmb))
	// A sibling whose claim is recorded AFTER its own attempt: the
	// claim/attempt order is backwards, so admission is unknowable even
	// though both timestamps predate the ambiguity.
	withTriggerRemoved(t, dm.SQLDB(), []string{"trg_agent_task_sessions_admission_guard", "trg_agent_task_sessions_epoch_is_current"}, func() {
		_, err := dm.SQLDB().Exec(`INSERT INTO agent_tasks
			(id, project_id, root_id, parent_id, depth, title, objective, recipient, role, state, idempotency_key)
			VALUES ('at-misordered', 'cpj-rt0000000001', (SELECT root_id FROM agent_tasks WHERE id = ?), ?, 1, 'm', 'o', 'alpha', 'implementer', 'enqueued', 'idem:mis')`, taskID, cur.ParentID)
		require.NoError(t, err)
		_, err = dm.SQLDB().Exec(`INSERT INTO agent_task_events
			(id, task_id, event_index, event_type, claim_epoch, created_at)
			VALUES ('ate-mis-claim', 'at-misordered', 0, 'claimed', 1, ?)`, tAmb+100)
		require.NoError(t, err)
		_, err = dm.SQLDB().Exec(`INSERT INTO agent_task_sessions
			(id, task_id, attempt, claim_epoch, runtime, intended_agent,
			native_session_id, correlation_ref, workspace_id, workspace_path,
			launch_state, runtime_outcome, created_at, updated_at)
			VALUES ('ats-mis', 'at-misordered', 1, 1, 'opencode', 'mpm-worker',
			'ses-mis', 'corr-mis', 'ws', '/tmp/ws', 'completed', 'succeeded', ?, ?)`,
			tAmb+50, tAmb+50)
		require.NoError(t, err)
	})
	dump := buildRecoveryDump(t, dm.SQLDB())
	restoreMarker(t, dm.SQLDB(), taskID)
	assertRestoreRefusedAndIntact(t, dm, dump, "indeterminate_temporal_order")
}

// canonicalTriggerTextFor returns the exact canonical CREATE TRIGGER
// text a live database carries for one trigger.
func canonicalTriggerTextFor(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var sqlText string
	require.NoError(t, db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&sqlText),
		"the live schema must carry %s", name)
	return sqlText
}

// TestRestoreAcceptsQuotedAndQualifiedTriggerNamesIsT110 proves B1's
// identifier equivalence end to end: a dump whose canonical trigger is
// written with a quoted, schema-qualified name (and a quoted ON target)
// restores, and the restored database carries the TRUSTED canonical
// text, not the dump's spelling.
func TestRestoreAcceptsQuotedAndQualifiedTriggerNamesIsT110(t *testing.T) {
	dm, _ := roundTripFixture(t)
	dump := buildRecoveryDump(t, dm.SQLDB())
	name := "trg_agent_tasks_reserved_key"
	canonical := canonicalTriggerTextFor(t, dm.SQLDB(), name)
	require.True(t, strings.Contains(dump, canonical), "the dump must carry the canonical text")

	quoted := strings.Replace(canonical,
		"CREATE TRIGGER "+name, `CREATE TRIGGER "main"."`+name+`"`, 1)
	quoted = strings.Replace(quoted, "ON agent_tasks", `ON "main"."agent_tasks"`, 1)
	require.NotEqual(t, canonical, quoted, "the rewrite must change the dump")

	dump = strings.Replace(dump, canonical, quoted, 1)
	require.Equal(t, 0, runRestoreDB(t, dm, dump),
		"a quoted, schema-qualified trigger name is a benign B1 difference")

	db := openRestored(t, dm.DBPath())
	var restored string
	require.NoError(t, db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&restored))
	require.Equal(t, canonical, restored,
		"the restore must install the TRUSTED canonical text, never the dump's spelling")
}

// TestRestoreAcceptsOperatorWhitespaceIsT116 proves a benign whitespace
// difference around a multi-character operator (||) is accepted, and the
// canonical text is what lands.
func TestRestoreAcceptsOperatorWhitespaceIsT116(t *testing.T) {
	dm, _ := roundTripFixture(t)
	dump := buildRecoveryDump(t, dm.SQLDB())
	name := "trg_agent_tasks_reserved_key"
	canonical := canonicalTriggerTextFor(t, dm.SQLDB(), name)

	tight := strings.ReplaceAll(canonical, "' || ", "'||")
	tight = strings.ReplaceAll(tight, " || L.", "||L.")
	require.NotEqual(t, canonical, tight, "the rewrite must remove the spacing")
	dump = strings.Replace(dump, canonical, tight, 1)

	require.Equal(t, 0, runRestoreDB(t, dm, dump),
		"whitespace around || is not a meaningful difference")
	db := openRestored(t, dm.DBPath())
	var restored string
	require.NoError(t, db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&restored))
	require.Equal(t, canonical, restored,
		"the canonical operator spacing is restored, not the dump's")
}

// TestRestoreTriggerTargetClassificationIsF11 proves an unrelated
// trigger that merely READS a managed table is not a managed-table
// trigger: it replays verbatim. A trigger DEFINED ON a managed table but
// outside the canonical set is still refused (T115b). Modified canonical
// definitions stay refused (T112).
func TestRestoreTriggerTargetClassificationIsF11(t *testing.T) {
	t.Run("unrelated trigger referencing a managed table is accepted", func(t *testing.T) {
		dm, _ := roundTripFixture(t)
		dump := buildRecoveryDump(t, dm.SQLDB())
		// `projects` exists in this dump; the trigger is defined ON it
		// and merely READS a managed table.
		unrelated := `CREATE TRIGGER projects_audit_managed AFTER INSERT ON projects
BEGIN
    SELECT COUNT(*) FROM agent_tasks;
END;`
		require.Equal(t, 0, runRestoreDB(t, dm, dump+string('\n')+unrelated+string('\n')),
			"a trigger on an unrelated table must replay, even when its body reads a managed table")
		db := openRestored(t, dm.DBPath())
		var sqlText string
		require.NoError(t, db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='projects_audit_managed'`).Scan(&sqlText))
		require.Contains(t, sqlText, "agent_tasks")
	})
	t.Run("unexpected trigger defined on a managed table is refused", func(t *testing.T) {
		dm, taskID := roundTripFixture(t)
		dump := buildRecoveryDump(t, dm.SQLDB())
		dump += `CREATE TRIGGER "main"."trg_agent_tasks_shadow" BEFORE UPDATE ON "main"."agent_tasks"
FOR EACH ROW BEGIN SELECT RAISE(ABORT, 'shadow'); END;
`
		restoreMarker(t, dm.SQLDB(), taskID)
		assertRestoreRefusedAndIntact(t, dm, dump, "unexpected_trigger")
	})
	t.Run("modified canonical definition is refused", func(t *testing.T) {
		dm, taskID := roundTripFixture(t)
		dump := buildRecoveryDump(t, dm.SQLDB())
		name := "trg_agent_tasks_reserved_key"
		canonical := canonicalTriggerTextFor(t, dm.SQLDB(), name)
		weakened := strings.Replace(canonical, "GLOB 'supersede:*'", "GLOB '*'", 1)
		require.NotEqual(t, canonical, weakened)
		dump = strings.Replace(dump, canonical, weakened, 1)
		restoreMarker(t, dm.SQLDB(), taskID)
		assertRestoreRefusedAndIntact(t, dm, dump, "modified_canonical_trigger")
	})
}

// TestRestoreAwareDumpMissingAGuardIsT114 proves a supersession-aware
// dump that omits ONE required canonical guard is refused with the
// missing-trigger diagnostic (T113 covers the general removal; this is
// the aware-class case the reviewer called out).
func TestRestoreAwareDumpMissingAGuardIsT114(t *testing.T) {
	dm, ids, _, _ := temporalFixture(t)
	dump := buildRecoveryDump(t, dm.SQLDB())
	name := "trg_agent_tasks_superseded_shape"
	text := canonicalTriggerText(t, dm.SQLDB(), name)
	stripped, _, found := strings.Cut(dump, text+";\n")
	require.True(t, found, "the aware dump must carry %s", name)
	require.NotContains(t, stripped, name)
	restoreMarker(t, dm.SQLDB(), ids["a"])
	assertRestoreRefusedAndIntact(t, dm, stripped, "missing_canonical_trigger")
}

// TestRestorePostFeatureZeroLineagesIsT118 proves classification is
// table+sentinel based: a supersession-aware database with ZERO lineage
// rows is aware (not pre-feature), so its canonical guards are
// REQUIRED, and it restores.
func TestRestorePostFeatureZeroLineagesIsT118(t *testing.T) {
	dm, taskID := roundTripFixture(t)
	require.Equal(t, 0, restoreRowCount(t, dm.SQLDB(), "agent_task_supersessions", ""),
		"the fixture must carry zero lineages")
	dump := buildRecoveryDump(t, dm.SQLDB())
	// Read the guard's text BEFORE the restore closes this manager.
	text := canonicalTriggerText(t, dm.SQLDB(), "trg_agent_task_sessions_completion_guard")
	require.Equal(t, 0, runRestoreDB(t, dm, dump),
		"a zero-lineage aware dump must restore")
	// It is classified aware: dropping a session-class guard is refused.
	// The success above closed dm, so the marker is written through a
	// fresh connection to the restored file.
	stripped, _, found := strings.Cut(dump, text+";\n")
	require.True(t, found)
	restored := openRestored(t, dm.DBPath())
	restoreMarker(t, restored, taskID)
	assertRestoreRefusedAndIntact(t, dm, stripped, "missing_canonical_trigger")
}

// TestRestoreScopedSiblingBeforeAmbiguityIsT128Accept proves C1's
// ACCEPT arm: an unscoped sibling whose claim event and attempt row both
// strictly predate the ambiguity is admitted, and its history survives.
func TestRestoreScopedSiblingBeforeAmbiguityIsT128Accept(t *testing.T) {
	dm, ids := roundTripFixture(t)
	store := assignment.NewStore(dm)
	actor := assignment.Actor{Address: "alpha", Kind: "agent"}
	cur, err := store.Get(ids)
	require.NoError(t, err)
	for _, step := range []func(assignment.FencedParams) (*assignment.Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		_, err := step(assignment.FencedParams{ID: ids, Actor: actor})
		require.NoError(t, err)
	}
	claimed, err := store.Claim(assignment.ClaimParams{
		ID: ids, ExpectedEpoch: cur.ClaimEpoch, Owner: "alpha", LeaseSeconds: 600, Actor: actor,
	})
	require.NoError(t, err)
	if _, err := store.Start(assignment.FencedParams{
		ID: ids, Epoch: claimed.ClaimEpoch, Actor: actor}); err != nil {
		t.Fatalf("start: %v", err)
	}
	// One second of separation so the ordering is unambiguous at the
	// storage's resolution.
	time.Sleep(1100 * time.Millisecond)
	att := startAmbiguousForTest(t, store, ids, claimed.ClaimEpoch, "ses-t128-acc")
	dump := buildRecoveryDump(t, dm.SQLDB())
	restoreMarker(t, dm.SQLDB(), ids)
	_ = att
	require.Equal(t, 0, runRestoreDB(t, dm, dump),
		"a claim and attempt both strictly before the ambiguity are admitted")
	db := openRestored(t, dm.DBPath())
	require.Equal(t, 1, restoreRowCount(t, db, "agent_task_sessions", "id = ?", att.ID),
		"the admitted attempt must survive")
}

// TestRestoreUnscopedAttemptAfterAmbiguityIsT128Refuse proves C1's
// REFUSE arm: an earlier claim does not authorise an attempt recorded at
// or after the ambiguity.
func TestRestoreUnscopedAttemptAfterAmbiguityIsT128Refuse(t *testing.T) {
	dm, ids := roundTripFixture(t)
	store := assignment.NewStore(dm)
	actor := assignment.Actor{Address: "alpha", Kind: "agent"}
	cur, err := store.Get(ids)
	require.NoError(t, err)
	for _, step := range []func(assignment.FencedParams) (*assignment.Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		_, err := step(assignment.FencedParams{ID: ids, Actor: actor})
		require.NoError(t, err)
	}
	claimed, err := store.Claim(assignment.ClaimParams{
		ID: ids, ExpectedEpoch: cur.ClaimEpoch, Owner: "alpha", LeaseSeconds: 600, Actor: actor,
	})
	require.NoError(t, err)
	if _, err := store.Start(assignment.FencedParams{
		ID: ids, Epoch: claimed.ClaimEpoch, Actor: actor}); err != nil {
		t.Fatalf("start: %v", err)
	}
	att := startAmbiguousForTest(t, store, ids, claimed.ClaimEpoch, "ses-t128-ref")
	var tAmb int64
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT updated_at FROM agent_task_sessions WHERE id = ?`, att.ID).Scan(&tAmb))
	// A second attempt on the same epoch recorded AT the ambiguity: the
	// claim predates it, the attempt does not.
	plantedAttempt(t, dm.SQLDB(), "ats-t128-late", ids, 2, claimed.ClaimEpoch,
		"completed", "succeeded", tAmb, tAmb)
	dump := buildRecoveryDump(t, dm.SQLDB())
	restoreMarker(t, dm.SQLDB(), ids)
	assertRestoreRefusedAndIntact(t, dm, dump, "post_ambiguity_unscoped_session")
}

// startAmbiguousForTest records an ambiguous attempt (the fixture names
// it separately so the T128 tests read clearly).
func startAmbiguousForTest(t *testing.T, store *assignment.Store, id string, epoch int64, ses string) *assignment.SessionAttempt {
	t.Helper()
	att, err := store.BeginAttempt(assignment.BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: assignment.RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: ses,
		CorrelationRef: "corr-" + ses, WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	require.NoError(t, err)
	if _, err := store.MarkAttemptLaunched(assignment.SessionStateParams{
		AttemptID: att.ID, Epoch: epoch}); err != nil {
		t.Fatalf("mark launched: %v", err)
	}
	if _, err := store.MarkAttemptAmbiguous(assignment.SessionStateParams{
		AttemptID: att.ID, Epoch: epoch, Error: "lost"}); err != nil {
		t.Fatalf("mark ambiguous: %v", err)
	}
	return att
}
