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
//  3. they yield a WRITABLE OBJECT, whatever the column holds — a
//     parseable but non-object document is normalised to the empty
//     object, so a path assignment always lands;
//  4. they read a malformed / NULL / empty document as UNTAGGED, which
//     is the reading Wake.Kind() already takes.
//
// A SEPARATE, CANONICAL RULE MARKS THE CORRUPT SHAPES. WakeMetadataIsCorrupt
// is true only for a document that is PRESENT, NON-EMPTY and NOT
// PARSEABLE. Such a row is owned by NO partition (issue #178, W4): it is
// never delivered, claimed, retired or resolved, and its original bytes
// are preserved because they are the only evidence of what it was. The
// Go twin, WakeMetadataIsCorruptValue, is used by the writers that
// already hold the row, and the two are pinned to each other by
// behaviour below — not by spelling.

package internal

import (
	"database/sql"
	"encoding/json"
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
// a runtime subset of the agent-owned kinds. Both must select the SAME
// ROWS for the same inputs, or the fold and the scheduler would
// disagree about which rows are notifications.
//
// The comparison is BEHAVIOURAL. Both expressions are evaluated by
// SQLite against the same fixture table and the two sets of matched
// rows are compared. A string comparison would fail on any legitimate
// refactor of the predicate's text — and it failed once already, on
// spelling rather than on behaviour — while a behavioural comparison fails
// only when the two genuinely disagree.
func TestWakeKindFiltersMatchTheFoldBuilder(t *testing.T) {
	db := openWakeMetadataTestDB(t)

	cases := []struct {
		name          string
		kinds         []string
		untaggedMatch bool
		want          string
	}{
		{"notification", []string{"notification"}, true, WakeNotificationKindFilter},
		{"cron", []string{"cron"}, false, WakeCronKindFilter},
		{"cascade_summary", []string{"cascade_summary"}, false, WakeCascadeSummaryKindFilter},
		{"notification_cascade", []string{"notification", "cascade"}, true, WakeNotificationAndCascadeKindFilter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			constantRows := wakeFilterMatchedLabels(t, db, c.want)
			builtRows := wakeFilterMatchedLabels(t, db, wakeKindSQL(c.kinds, c.untaggedMatch))
			if !wakeSameStringSet(constantRows, builtRows) {
				t.Errorf("the exported %s filter and the fold builder select different rows:\n"+
					"  exported: %v\n  built:    %v", c.name, constantRows, builtRows)
			}
			if len(constantRows) == 0 {
				t.Errorf("the exported %s filter matched no row at all: the comparison would be vacuous",
					c.name)
			}
		})
	}

	// NEGATIVE CONTROL. The comparison must be able to FAIL. A filter
	// that differs in exactly one rule — whether an untagged row counts —
	// must select a DIFFERENT set of rows. If it did not, the
	// comparison above would pass for any pair of expressions.
	constantRows := wakeFilterMatchedLabels(t, db, WakeCronKindFilter)
	widerRows := wakeFilterMatchedLabels(t, db, wakeKindSQL([]string{"cron"}, true))
	if wakeSameStringSet(constantRows, widerRows) {
		t.Errorf("the negative control selected the same rows (%v): the behavioural "+
			"comparison cannot distinguish the two filters", constantRows)
	}
}

// wakeFilterMatchedLabels returns the labels of the rows the given
// filter expression selects.
func wakeFilterMatchedLabels(t *testing.T, db *sql.DB, filter string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT label FROM wake_meta_shapes WHERE ` + filter + ` ORDER BY label`)
	if err != nil {
		t.Fatalf("evaluate the filter %q: %v", filter, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, label)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

// wakeSameStringSet reports whether two label sets are equal, order
// independent.
func wakeSameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		seen[v]--
		if seen[v] < 0 {
			return false
		}
	}
	return true
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
	// wantCorrupt is the expected result of the corruption predicate:
	// a document that is present, non-empty and NOT parseable.
	wantCorrupt bool
}

// wakeMetadataShapes is the full set of inputs the helpers must
// handle. Every one of them must read as UNTAGGED unless it carries
// an explicit kind.
var wakeMetadataShapes = []wakeMetadataShape{
	{"malformed object", `{"kind": "notification"`, "", 1, 0, true},
	{"malformed, not JSON at all", `notification`, "", 1, 0, true},
	{"malformed, trailing garbage", `{"kind":"notification"} trailing`, "", 1, 0, true},
	{"malformed, whitespace only", `   `, "", 1, 0, true},
	{"NULL", nil, "", 1, 0, false},
	{"empty string", "", "", 1, 0, false},
	{"empty object", `{}`, "", 1, 0, false},
	// THE VALID NON-OBJECT SHAPES ARE HERE ON PURPOSE. The first guard
	// kept every parseable document, and json_set() on a non-object
	// document returns it UNCHANGED — silently dropping the key. The
	// write "landed" with no audit note and the sweep's read-back
	// assertion then failed, aborting the whole batch. These are the
	// shapes that let that through.
	{"null literal", `null`, "", 1, 0, false},
	{"number literal", `42`, "", 1, 0, false},
	{"string literal", `"str"`, "", 1, 0, false},
	{"boolean literal", `true`, "", 1, 0, false},
	{"empty array", `[]`, "", 1, 0, false},
	{"array of objects", `[{"kind":"cron"}]`, "", 1, 0, false},
	{"null kind", `{"kind":null}`, "", 1, 0, false},
	{"empty kind", `{"kind":""}`, "", 1, 0, false},
	// A kind that is not a string is a DISTINCT kind, not an untagged
	// one: it matches neither filter, exactly as the pre-fix CASE
	// form behaved.
	{"kind is not a string", `{"kind":42}`, "42", 0, 0, false},
	{"valid notification", `{"kind":"notification"}`, "notification", 1, 0, false},
	{"valid cron", `{"kind":"cron"}`, "cron", 0, 1, false},
	{"valid snapshot", `{"kind":"snapshot"}`, "snapshot", 0, 0, false},
	{"valid cascade summary", `{"kind":"cascade_summary"}`, "cascade_summary", 0, 0, false},
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

// TestWakeMetadataIsCorruptSQLAgreesWithTheGoPredicate pins the two
// implementations of the corruption rule to each other.
//
// The SQL predicate is used by the claim, the sweeps, the fold and the
// resolver; the Go predicate is used by the writers that already hold
// the candidate row. If the two disagreed, a row could be rejected by
// one surface and retired by another — which is exactly the partition
// shift the policy forbids.
//
// The test evaluates BOTH against the same fixture table — the SQL
// predicate by SQLite, the Go predicate by the function — for every shape
// in the table. It is a BEHAVIOURAL comparison, not a spelling one.
func TestWakeMetadataIsCorruptSQLAgreesWithTheGoPredicate(t *testing.T) {
	db := openWakeMetadataTestDB(t)

	for _, shape := range wakeMetadataShapes {
		t.Run(shape.name, func(t *testing.T) {
			var sqlSays int
			if err := db.QueryRow(
				`SELECT CASE WHEN `+WakeMetadataIsCorrupt+` THEN 1 ELSE 0 END
				 FROM wake_meta_shapes WHERE label = ?`,
				shape.name,
			).Scan(&sqlSays); err != nil {
				t.Fatalf("evaluate the corruption predicate: %v", err)
			}

			var raw sql.NullString
			if err := db.QueryRow(
				`SELECT metadata FROM wake_meta_shapes WHERE label = ?`, shape.name,
			).Scan(&raw); err != nil {
				t.Fatalf("read the shape's metadata: %v", err)
			}
			goSays := WakeMetadataIsCorruptValue(raw)

			if (sqlSays == 1) != goSays {
				t.Errorf("the SQL predicate says corrupt=%v but the Go predicate says corrupt=%v "+
					"(metadata %v)", sqlSays == 1, goSays, shape.metadata)
			}
			if goSays != shape.wantCorrupt {
				t.Errorf("the shape is corrupt=%v, want %v (metadata %v)",
					goSays, shape.wantCorrupt, shape.metadata)
			}
		})
	}
}

// TestWakeMetadataJSONIsWritableForEveryShape is the regression for
// the guard's SECOND half.
//
// The first guard guaranteed that the document PARSES. It did not
// guarantee that a path assignment LANDS: json_set() on a non-object
// document returns the document unchanged, silently dropping the key.
// The write "landed" (one row affected) and carried no audit note, so
// the sweep's own read-back assertion failed and the whole batch was
// rolled back.
//
// The guard must yield a WRITABLE OBJECT for every shape.
func TestWakeMetadataJSONIsWritableForEveryShape(t *testing.T) {
	db := openWakeMetadataTestDB(t)

	for _, shape := range wakeMetadataShapes {
		t.Run(shape.name, func(t *testing.T) {
			var out string
			if err := db.QueryRow(
				`SELECT json_set(`+WakeMetadataJSON+`, '$.probe', 'landed')
				 FROM wake_meta_shapes WHERE label = ?`,
				shape.name,
			).Scan(&out); err != nil {
				t.Fatalf("json_set over the guarded document: %v", err)
			}
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(out), &m); err != nil {
				t.Fatalf("the guarded document is not a JSON object (%q): %v", out, err)
			}
			if m["probe"] != "landed" {
				t.Errorf("json_set did not land the key: got %q", out)
			}
		})
	}
}

// TestWakeMetadataJSONPreservesARealObject pins the other half of the
// guard: a document that IS an object must survive verbatim, kind and
// all. The guard normalises the NON-object shapes; it must not touch a
// real one.
func TestWakeMetadataJSONPreservesARealObject(t *testing.T) {
	db := openWakeMetadataTestDB(t)

	var out string
	if err := db.QueryRow(
		`SELECT ` + WakeMetadataJSON + ` FROM wake_meta_shapes WHERE label = 'valid notification'`,
	).Scan(&out); err != nil {
		t.Fatalf("read the guarded document: %v", err)
	}
	if out != `{"kind":"notification"}` {
		t.Errorf("the guarded document = %q, want %q", out, `{"kind":"notification"}`)
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
	if len(overdue) != 1 {
		t.Fatalf("gathered %d overdue wakes, want 1: a corrupt row must not hide the healthy one", len(overdue))
	}
	// The healthy row is still reported, with its kind.
	if overdue[0].Kind != "notification" {
		t.Errorf("the healthy row's kind = %q, want %q", overdue[0].Kind, "notification")
	}
	// The CORRUPT ROW IS NOT REPORTED AT ALL. A corrupt document reads
	// as untagged, so including it would present a scheduler-owned row
	// to the agent as an ordinary notification — the reclassification the
	// W4 policy forbids.
	for _, w := range overdue {
		if w.ID == "wk-corrupt" {
			t.Errorf("the corrupt row was gathered as an overdue wake (kind %q): "+
				"a corrupt row is owned by no partition", w.Kind)
		}
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

// TestHealthCheckCountsCorruptWakeMetadata pins the operator-visible
// diagnostic channel.
//
// The retention sweeps report a corrupt row when they MEET one, but a
// corrupt row that is not yet old enough for a sweep is invisible to
// them. The wakes_corrupt_metadata counter is the always-available
// signal: it counts every corrupt row, at any age, fired or not.
func TestHealthCheckCountsCorruptWakeMetadata(t *testing.T) {
	dm := NewTestDM(t)

	var nowUnix int64
	if err := dm.db.QueryRow(`SELECT CAST(strftime('%s','now') AS INTEGER)`).Scan(&nowUnix); err != nil {
		t.Fatalf("read the clock: %v", err)
	}

	seedCoreWake(t, dm, "wk-corrupt", nowUnix-120, `{"kind": "cron"`)
	seedCoreWake(t, dm, "wk-healthy", nowUnix-60, `{"kind":"notification"}`)

	hc, err := dm.HealthCheck()
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	n, ok := hc["wakes_corrupt_metadata"].(int64)
	if !ok {
		t.Fatalf("the health check carries no wakes_corrupt_metadata counter: %v", hc)
	}
	if n != 1 {
		t.Errorf("wakes_corrupt_metadata = %d, want 1", n)
	}

	// NEGATIVE CONTROL. The counter must fall when the row is
	// repaired, or it would be a constant rather than a measurement.
	if _, err := dm.db.Exec(
		`UPDATE scheduled_wakes SET metadata = '{"kind":"cron"}' WHERE id = 'wk-corrupt'`); err != nil {
		t.Fatalf("repair the row: %v", err)
	}
	hc, err = dm.HealthCheck()
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	n, _ = hc["wakes_corrupt_metadata"].(int64)
	if n != 0 {
		t.Errorf("wakes_corrupt_metadata = %d after the row was repaired, want 0", n)
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
