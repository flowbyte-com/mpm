// Tests for the always-valid wake-metadata SQL idiom
// (internal/core/wake_tools.go: WakeMetadataJSON, WakeKindExpr and the
// canonical kind filters).
//
// These are the primitives the W4 fix is built on. Every production
// predicate that reads a wake's `kind` out of possibly-malformed
// metadata goes through them, so their contract is:
//
//  1. they never RAISE, whatever the column holds;
//  2. they never depend on SQL evaluation order — the JSON function is
//     never handed the raw column, so no OR-chain short-circuit is
//     needed for safety;
//  3. they read a malformed / NULL / empty document as UNTAGGED, which
//     is the reading Wake.Kind() already takes.

package internal

import (
	"database/sql"
	"strings"
	"testing"
)

// ── the helper's own contract ─────────────────────────────────────────────

// TestWakeMetadataSQLNeverHandsTheRawColumnToAJSONFunction is the
// structural half of the contract.
//
// If the generated expression ever contains `json_extract(metadata` —
// i.e. the JSON function applied to the RAW column — then it can raise
// on a malformed document, and its safety would depend on the order in
// which SQLite happens to evaluate the surrounding expression. That is
// exactly the unproven assumption the fix must not make.
//
// The expression must therefore route the column through json_valid
// first.
func TestWakeMetadataSQLNeverHandsTheRawColumnToAJSONFunction(t *testing.T) {
	if strings.Contains(WakeKindExpr, "json_extract(metadata") {
		t.Errorf("the kind expression hands the raw column to json_extract: %s", WakeKindExpr)
	}
	if !strings.Contains(WakeKindExpr, "json_valid(metadata)") {
		t.Errorf("the kind expression does not probe json_valid: %s", WakeKindExpr)
	}
	if !strings.Contains(WakeKindExpr, "ELSE '{}'") {
		t.Errorf("the kind expression has no always-valid fallback: %s", WakeKindExpr)
	}

	if strings.Contains(WakeMetadataJSON, "json_extract(metadata") {
		t.Errorf("the document expression hands the raw column to a JSON function: %s", WakeMetadataJSON)
	}
	if !strings.Contains(WakeMetadataJSON, "json_valid(metadata)") {
		t.Errorf("the document expression does not probe json_valid: %s", WakeMetadataJSON)
	}
}

// TestWakeKindSQLHasNoOrderDependentORChain pins the second half of
// the structural contract.
//
// An OR-chain of json_extract() calls is only safe if SQLite
// short-circuits it, and the order of an OR's operands is not
// specified. The generated filters must be a single IN test over the
// always-valid extraction.
func TestWakeKindSQLHasNoOrderDependentORChain(t *testing.T) {
	filters := map[string]string{
		"notification":         WakeNotificationKindFilter,
		"cron":                 WakeCronKindFilter,
		"cascade_summary":      WakeCascadeSummaryKindFilter,
		"notification_cascade": WakeNotificationAndCascadeKindFilter,
	}
	for name, filter := range filters {
		if strings.Contains(filter, " OR ") {
			t.Errorf("the %s kind filter is an OR-chain, which depends on evaluation order: %s", name, filter)
		}
		if strings.Contains(filter, "json_extract(metadata") {
			t.Errorf("the %s kind filter hands the raw column to json_extract: %s", name, filter)
		}
		if !strings.Contains(filter, " IN (") {
			t.Errorf("the %s kind filter is not a single IN test: %s", name, filter)
		}
	}
}

// TestWakeKindFiltersMatchTheFoldBuilder is the anti-drift guard.
//
// The exported filters are compile-time constants; the fold's builder
// is a function, because the fold's "specific list" branch filters on
// a runtime subset of the agent-owned kinds. Both must produce the
// SAME text for the same inputs, or the fold and the scheduler would
// disagree about which rows are notifications.
func TestWakeKindFiltersMatchTheFoldBuilder(t *testing.T) {
	cases := []struct {
		name           string
		kinds          []string
		untaggedMatch  bool
		want           string
	}{
		{"notification", []string{"notification"}, true, WakeNotificationKindFilter},
		{"cron", []string{"cron"}, false, WakeCronKindFilter},
		{"cascade_summary", []string{"cascade_summary"}, false, WakeCascadeSummaryKindFilter},
		{"notification_cascade", []string{"notification", "cascade"}, true, WakeNotificationAndCascadeKindFilter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wakeKindSQL(c.kinds, c.untaggedMatch)
			if got != c.want {
				t.Errorf("wakeKindSQL(%v, %v) = %q, want %q", c.kinds, c.untaggedMatch, got, c.want)
			}
		})
	}
}

// ── the helper's behaviour, against a real database ───────────────────────

// wakeMetadataShape is one input the helpers must survive.
type wakeMetadataShape struct {
	name     string
	metadata any
	// wantKind is the kind the expression must read. A malformed,
	// NULL or empty document reads as UNTAGGED ('').
	wantKind string
	// wantNotif is the expected result of the notification filter
	// (untagged counts as a match).
	wantNotif int
	// wantCron is the expected result of the cron filter (untagged
	// does NOT count — the kind boundary must not widen).
	wantCron int
}

// wakeMetadataShapes is the full set of inputs the helpers must
// handle. Every one of them must read as UNTAGGED unless it carries
// an explicit kind.
var wakeMetadataShapes = []wakeMetadataShape{
	{"malformed object", `{"kind": "notification"`, "", 1, 0},
	{"malformed, not JSON at all", `notification`, "", 1, 0},
	{"malformed, trailing garbage", `{"kind":"notification"} trailing`, "", 1, 0},
	{"NULL", nil, "", 1, 0},
	{"empty string", "", "", 1, 0},
	{"empty object", `{}`, "", 1, 0},
	{"null kind", `{"kind":null}`, "", 1, 0},
	{"empty kind", `{"kind":""}`, "", 1, 0},
	// A kind that is not a string is a DISTINCT kind, not an untagged
	// one: it matches neither filter, exactly as the pre-fix CASE
	// form behaved.
	{"kind is not a string", `{"kind":42}`, "42", 0, 0},
	{"valid notification", `{"kind":"notification"}`, "notification", 1, 0},
	{"valid cron", `{"kind":"cron"}`, "cron", 0, 1},
	{"valid snapshot", `{"kind":"snapshot"}`, "snapshot", 0, 0},
}

// TestWakeKindExprSQLReadsEveryShape is the behavioural contract.
//
// The expression is evaluated by SQLite itself — not by a Go
// re-implementation of the rule — so the test proves the SQL, not the
// intent.
func TestWakeKindExprSQLReadsEveryShape(t *testing.T) {
	db := openWakeMetadataTestDB(t)

	for _, shape := range wakeMetadataShapes {
		t.Run(shape.name, func(t *testing.T) {
			var got string
			err := db.QueryRow(
				`SELECT `+WakeKindExpr+` FROM wake_meta_shapes WHERE label = ?`,
				shape.name,
			).Scan(&got)
			if err != nil {
				t.Fatalf("evaluate the kind expression: %v", err)
			}
			if got != shape.wantKind {
				t.Errorf("kind = %q, want %q (metadata %v)", got, shape.wantKind, shape.metadata)
			}
		})
	}
}

// TestWakeKindFilterSQLMatchesTheExpectedRows is the behavioural
// contract for the filters.
//
// Two filters are checked:
//
//   - the notification filter (untagged counts as a match) — the
//     scheduler's claim and the 7-day sweep;
//   - the cron filter (untagged does NOT count) — the 1-hour sweep,
//     whose kind boundary must not widen.
func TestWakeKindFilterSQLMatchesTheExpectedRows(t *testing.T) {
	db := openWakeMetadataTestDB(t)

	for _, shape := range wakeMetadataShapes {
		t.Run(shape.name, func(t *testing.T) {
			var notif, cron int
			err := db.QueryRow(
				`SELECT (`+WakeNotificationKindFilter+`), (`+WakeCronKindFilter+`)
				 FROM wake_meta_shapes WHERE label = ?`,
				shape.name,
			).Scan(&notif, &cron)
			if err != nil {
				t.Fatalf("evaluate the kind filter: %v", err)
			}
			if notif != shape.wantNotif {
				t.Errorf("notification filter = %d, want %d (metadata %v)", notif, shape.wantNotif, shape.metadata)
			}
			if cron != shape.wantCron {
				t.Errorf("cron filter = %d, want %d (metadata %v)", cron, shape.wantCron, shape.metadata)
			}
		})
	}
}

// ── the agent-facing readers ──────────────────────────────────────────────

// TestGatherOverdueWakesSurvivesMalformedMetadata covers the
// agent-facing overdue list.
//
// The reader extracted the kind with a bare json_extract(), so a
// corrupt row raised and the agent's wake context lost EVERY overdue
// wake — the healthy ones included.
func TestGatherOverdueWakesSurvivesMalformedMetadata(t *testing.T) {
	dm := NewTestDM(t)

	now := dm.db.QueryRow(`SELECT CAST(strftime('%s','now') AS INTEGER)`)
	var nowUnix int64
	if err := now.Scan(&nowUnix); err != nil {
		t.Fatalf("read the clock: %v", err)
	}

	// The corrupt row is older, so it comes first.
	seedCoreWake(t, dm, "wk-corrupt", nowUnix-120, `{"kind": "notification"`)
	seedCoreWake(t, dm, "wk-healthy", nowUnix-60, `{"kind":"notification"}`)

	overdue := dm.gatherOverdueWakes()
	if len(overdue) != 2 {
		t.Fatalf("gathered %d overdue wakes, want 2: a corrupt row must not hide the healthy one", len(overdue))
	}
	// The corrupt row reads as untagged.
	if overdue[0].Kind != "" {
		t.Errorf("the corrupt row's kind = %q, want %q (untagged)", overdue[0].Kind, "")
	}
	if overdue[1].Kind != "notification" {
		t.Errorf("the healthy row's kind = %q, want %q", overdue[1].Kind, "notification")
	}
}

// TestLastCascadeSummaryMetricsSurvivesMalformedMetadata covers the
// cascade-summary dedupe read.
//
// The read looked for kind='cascade_summary' with a bare
// json_extract(), so a corrupt row raised and the caller could never
// read the previous summary again.
func TestLastCascadeSummaryMetricsSurvivesMalformedMetadata(t *testing.T) {
	dm := NewTestDM(t)

	now := dm.db.QueryRow(`SELECT CAST(strftime('%s','now') AS INTEGER)`)
	var nowUnix int64
	if err := now.Scan(&nowUnix); err != nil {
		t.Fatalf("read the clock: %v", err)
	}

	// A corrupt row that is OLDER than the real summary.
	seedCoreWake(t, dm, "wk-corrupt", nowUnix-120, `{"kind": "cascade_summary"`)
	seedCoreWake(t, dm, "wk-summary", nowUnix-60, `{"kind":"cascade_summary","materialized":7,"failed":1,"pending_after":2,"elapsed_ms":42}`)

	materialized, failed, pendingAfter, elapsedMs, ok, err := dm.LastCascadeSummaryMetrics(t.Context())
	if err != nil {
		t.Fatalf("a malformed metadata row must not abort the cascade-summary read: %v", err)
	}
	if !ok {
		t.Fatalf("the cascade summary was not found: the corrupt row must not hide it")
	}
	if materialized != 7 || failed != 1 || pendingAfter != 2 || elapsedMs != 42 {
		t.Errorf("metrics = (%d, %d, %d, %d), want (7, 1, 2, 42)", materialized, failed, pendingAfter, elapsedMs)
	}
}

// ── the fixture ───────────────────────────────────────────────────────────

// openWakeMetadataTestDB returns a disposable database holding one
// row per metadata shape.
func openWakeMetadataTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open the disposable db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE wake_meta_shapes (
		label    TEXT PRIMARY KEY,
		metadata JSON
	)`); err != nil {
		t.Fatalf("create the fixture table: %v", err)
	}
	for _, shape := range wakeMetadataShapes {
		if _, err := db.Exec(
			`INSERT INTO wake_meta_shapes (label, metadata) VALUES (?, ?)`,
			shape.name, shape.metadata,
		); err != nil {
			t.Fatalf("seed the shape %q: %v", shape.name, err)
		}
	}
	return db
}

// seedCoreWake inserts a wake row with an arbitrary metadata value.
func seedCoreWake(t *testing.T, dm *DatabaseManager, id string, targetUnix int64, metadata any) {
	t.Helper()
	if _, err := dm.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, ?, 0, 'test', ?)`,
		id, targetUnix, "test-wake "+id, metadata,
	); err != nil {
		t.Fatalf("seed wake %s: %v", id, err)
	}
}
