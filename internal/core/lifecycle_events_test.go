// lifecycle_events_test.go — the storage contract for the mpm-event/0.1
// lifecycle store (P5).
//
// The design and its rationale are in
// docs/dev/session-a-p5-lifecycle-ingest.md. These tests are the
// executable half of that document: if the DDL and the contract drift, one
// of them fails.
//
// Every test pins an isolated HOME, MPM_WORKSPACE and MPM_DB_PATH and
// opens a disposable file-backed database, so the production path
// resolution is exercised rather than bypassed, and the live database is
// never touched.

package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ── Fixtures ─────────────────────────────────────────────────────────

// lifecycleEnvelope builds a VALID mpm-event/0.1 envelope.
//
// The defaults are the shape the reference adapter emits: a session
// observation with no operation behind it, and a meta bag carrying the
// host's own event type and the host's per-call correlation id.
func lifecycleEnvelope() map[string]interface{} {
	return map[string]interface{}{
		"schema_version":       LifecycleEventSchemaVersion,
		"event_id":             "ev1_" + strings.Repeat("a", 64),
		"idempotency":          "strict",
		"event_type":           "session.observed",
		"observation":          "observed",
		"observed_at_ms":       int64(1793021872123),
		"host":                 "opencode",
		"host_version":         "2.1.4",
		"source":               "opencode-plugin",
		"adapter_version":      "abc1234",
		"project_id":           nil,
		"project_id_source":    "unresolved",
		"framework_name":       "opencode",
		"framework_session_id": "ses_abc",
		"operation":            "absent",
		"result_status":        "unknown",
		"trust":                "observation",
		"meta": map[string]interface{}{
			"host_event_type": "session.text.delta",
			"host_call_id":    "call_shared",
		},
		"payload": nil,
	}
}

// lifecycleToolEnvelope builds a VALID envelope whose observation IS the
// recorded result of an operation (§3.7: operation=present).
func lifecycleToolEnvelope() map[string]interface{} {
	env := lifecycleEnvelope()
	env["event_id"] = "ev1_" + strings.Repeat("b", 64)
	env["event_type"] = "tool.outcome"
	env["operation"] = "present"
	env["result_status"] = "success"
	env["outcome_class"] = "ok"
	return env
}

// lifecycleIngest ingests an envelope and fails the test on error.
func lifecycleIngest(t *testing.T, dm *DatabaseManager, raw map[string]interface{}) LifecycleEventReceipt {
	t.Helper()
	receipt, err := IngestLifecycleEvent(dm.SQLDB(), raw)
	if err != nil {
		t.Fatalf("IngestLifecycleEvent: %v", err)
	}
	return receipt
}

// lifecycleRowCount counts the rows in lifecycle_events.
func lifecycleRowCount(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM lifecycle_events`).Scan(&n); err != nil {
		t.Fatalf("count lifecycle_events: %v", err)
	}
	return n
}

// lifecycleRejectionReason asserts that err is a typed lifecycle rejection
// with the given reason, and returns it.
func lifecycleRejectionReason(t *testing.T, err error, want string) *LifecycleEventRejection {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a rejection with reason %q, got no error", want)
	}
	rej, ok := err.(*LifecycleEventRejection)
	if !ok {
		t.Fatalf("expected a *LifecycleEventRejection, got %T: %v", err, err)
	}
	if rej.Reason != want {
		t.Fatalf("expected reason %q, got %q (%s)", want, rej.Reason, rej.Detail)
	}
	return rej
}

// ── The storage contract ─────────────────────────────────────────────

// TestLifecycleEvents_TableIsCreatedByBaseTables pins that the table is
// created by the ordinary schema path — that the migration is
// SELF-APPLYING and needs no separate step.
func TestLifecycleEvents_TableIsCreatedByBaseTables(t *testing.T) {
	dm := NewTestDM(t)

	var name string
	err := dm.SQLDB().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='lifecycle_events'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("lifecycle_events must exist after InitSchema: %v", err)
	}

	for _, idx := range []string{
		"idx_lifecycle_events_order",
		"idx_lifecycle_events_session",
		"idx_lifecycle_events_type",
	} {
		var got string
		if err := dm.SQLDB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx,
		).Scan(&got); err != nil {
			t.Errorf("index %s must exist after InitSchema: %v", idx, err)
		}
	}
}

// TestLifecycleEvents_SchemaMatchesTheContract pins the DDL against the
// contract, so the table and the document cannot drift.
//
// It checks the columns that carry the contract's semantics, and the
// CHECK constraints on the four vocabularies that are closed. The two
// vocabularies that are EXPECTED to widen — event_type and outcome_class
// — are checked to have NO check constraint, because a check would make a
// minor version bump require a table rebuild.
func TestLifecycleEvents_SchemaMatchesTheContract(t *testing.T) {
	dm := NewTestDM(t)

	rows, err := dm.SQLDB().Query(`PRAGMA table_info(lifecycle_events)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	type col struct {
		name    string
		notNull bool
		dflt    sql.NullString
		primary bool
	}
	cols := map[string]col{}
	for rows.Next() {
		var c col
		var cid int
		var ctype string
		var pk int
		if err := rows.Scan(&cid, &c.name, &ctype, &c.notNull, &c.dflt, &pk); err != nil {
			rows.Close()
			t.Fatalf("table_info scan: %v", err)
		}
		c.primary = pk > 0
		cols[c.name] = c
	}
	rows.Close()

	// Every column the contract's envelope carries, and the ones the
	// store adds.
	for _, name := range []string{
		"event_id", "idempotency", "schema_version", "event_type",
		"observation", "trust", "observed_at_ms", "ingested_at_ms",
		"host_seq", "host", "host_version", "source", "adapter_version",
		"project_id", "project_id_source", "framework_name",
		"framework_session_id", "mpm_session_id", "invocation_id",
		"task_id", "parent_event_id", "operation", "result_status",
		"outcome_class", "outcome_code", "meta",
	} {
		if _, ok := cols[name]; !ok {
			t.Errorf("the table must have a %q column", name)
		}
	}

	// The primary key IS the event id.
	if c, ok := cols["event_id"]; ok && !c.primary {
		t.Error("event_id must be the PRIMARY KEY — the key IS the dedup mechanism")
	}
	// The required columns must be NOT NULL.
	for _, name := range []string{
		"event_id", "idempotency", "schema_version", "event_type",
		"observation", "trust", "observed_at_ms", "ingested_at_ms",
		"host", "source", "adapter_version", "framework_name",
		"operation", "result_status", "meta",
	} {
		if c, ok := cols[name]; ok && !c.notNull {
			t.Errorf("%q must be NOT NULL", name)
		}
	}
	// The nullable ones must be nullable — a NOT NULL column that is
	// always NULL is persisted fiction.
	for _, name := range []string{
		"host_seq", "host_version", "project_id", "project_id_source",
		"framework_session_id", "mpm_session_id", "invocation_id",
		"task_id", "parent_event_id", "outcome_class", "outcome_code",
	} {
		if c, ok := cols[name]; ok && c.notNull {
			t.Errorf("%q must be NULLABLE — the envelope's value is optional", name)
		}
	}

	// The CHECK constraints: which vocabularies are pinned, and which
	// are deliberately left open.
	var ddl string
	if err := dm.SQLDB().QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='lifecycle_events'`,
	).Scan(&ddl); err != nil {
		t.Fatalf("read the DDL: %v", err)
	}
	for _, want := range []string{
		"idempotency IN ('strict','best_effort')",
		"trust = 'observation'",
		"operation IN ('present','absent')",
		"result_status IN ('success','error','unknown')",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("the DDL must pin %q", want)
		}
	}
	// event_type, outcome_class and observation must NOT be pinned: the
	// contract says each of those widens on a version bump, and a check
	// would make a minor bump require a table rebuild.
	for _, open := range []string{"event_type IN", "outcome_class IN", "observation IN"} {
		if strings.Contains(ddl, open) {
			t.Errorf("the DDL must NOT pin %q — that vocabulary widens on a version bump", open)
		}
	}
}

// TestLifecycleEvents_NoPayloadColumn pins §3.8: the envelope's payload
// is reserved and always null in 0.1, so the store has no payload
// column. A permanently-null column is persisted fiction.
func TestLifecycleEvents_NoPayloadColumn(t *testing.T) {
	dm := NewTestDM(t)

	var n int
	err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('lifecycle_events') WHERE name='payload'`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	if n != 0 {
		t.Error("the table must NOT have a payload column — it is always null in 0.1")
	}
}

// TestLifecycleEvents_RollbackDropsOnlyThisTable pins the rollback: the
// migration is reversible by dropping exactly one table and three
// indexes, and it touches nothing else.
func TestLifecycleEvents_RollbackDropsOnlyThisTable(t *testing.T) {
	dm := NewTestDM(t)

	before := map[string]int{}
	for _, table := range []string{"memories", "tool_invocations", "evidence", "system_audit_log", "scheduled_wakes"} {
		var n int
		if err := dm.SQLDB().QueryRow(
			fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		before[table] = n
	}

	rollback := []string{
		`DROP INDEX IF EXISTS idx_lifecycle_events_type`,
		`DROP INDEX IF EXISTS idx_lifecycle_events_session`,
		`DROP INDEX IF EXISTS idx_lifecycle_events_order`,
		`DROP TABLE IF EXISTS lifecycle_events`,
	}
	for _, stmt := range rollback {
		if _, err := dm.SQLDB().Exec(stmt); err != nil {
			t.Fatalf("rollback statement %q: %v", stmt, err)
		}
	}

	var gone int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE name='lifecycle_events'`).Scan(&gone); err != nil {
		t.Fatalf("check the table is gone: %v", err)
	}
	if gone != 0 {
		t.Error("the rollback must drop the table")
	}

	for table, want := range before {
		var got int
		if err := dm.SQLDB().QueryRow(
			fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&got); err != nil {
			t.Fatalf("count %s after rollback: %v", table, err)
		}
		if got != want {
			t.Errorf("the rollback changed %s: %d rows, want %d", table, got, want)
		}
	}

	// Re-applying is free: the DDL is CREATE TABLE IF NOT EXISTS.
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("re-apply the schema: %v", err)
	}
	var back int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE name='lifecycle_events'`).Scan(&back); err != nil {
		t.Fatalf("check the table is back: %v", err)
	}
	if back != 1 {
		t.Error("re-applying the schema must recreate the table")
	}
}

// ── The ingest contract (§3.10) ──────────────────────────────────────

// TestEventIngest_StoresAValidEnvelope is the happy path.
func TestEventIngest_StoresAValidEnvelope(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()

	receipt := lifecycleIngest(t, dm, env)

	if receipt.EventID != env["event_id"].(string) {
		t.Errorf("the receipt's event_id = %q, want %q", receipt.EventID, env["event_id"])
	}
	if receipt.Duplicate {
		t.Error("the first ingest must not be a duplicate")
	}
	if receipt.IngestedAtMs <= 0 {
		t.Errorf("the receipt's ingested_at_ms = %d, want a positive stamp", receipt.IngestedAtMs)
	}
	if got := lifecycleRowCount(t, dm); got != 1 {
		t.Fatalf("stored rows = %d, want 1", got)
	}

	// The stored row carries the envelope's fields.
	var (
		eventType, observation, trust, frameworkSession string
		observedAt                                      int64
		metaJSON                                        string
	)
	err := dm.SQLDB().QueryRow(`
		SELECT event_type, observation, trust, framework_session_id, observed_at_ms, meta
		FROM lifecycle_events WHERE event_id = ?`, env["event_id"]).Scan(
		&eventType, &observation, &trust, &frameworkSession, &observedAt, &metaJSON)
	if err != nil {
		t.Fatalf("read back the stored row: %v", err)
	}
	if eventType != "session.observed" {
		t.Errorf("stored event_type = %q", eventType)
	}
	if observation != "observed" {
		t.Errorf("stored observation = %q", observation)
	}
	if trust != "observation" {
		t.Errorf("stored trust = %q", trust)
	}
	if frameworkSession != "ses_abc" {
		t.Errorf("stored framework_session_id = %q", frameworkSession)
	}
	if observedAt != 1793021872123 {
		t.Errorf("stored observed_at_ms = %d, want the adapter's value", observedAt)
	}

	// The meta bag is stored VERBATIM — the core never rewrites a value.
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		t.Fatalf("the stored meta must be valid JSON: %v", err)
	}
	if meta["host_call_id"] != "call_shared" {
		t.Errorf("the stored meta must carry host_call_id verbatim, got %v", meta)
	}
	if meta["host_event_type"] != "session.text.delta" {
		t.Errorf("the stored meta must carry host_event_type verbatim, got %v", meta)
	}
}

// TestEventIngest_ReceiptIsReadBackFromTheStoredRow pins §6.4: the
// receipt's timestamp is the value the ROW holds, not the value the
// caller hoped to write.
func TestEventIngest_ReceiptIsReadBackFromTheStoredRow(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()

	receipt := lifecycleIngest(t, dm, env)

	var storedAt int64
	if err := dm.SQLDB().QueryRow(
		`SELECT ingested_at_ms FROM lifecycle_events WHERE event_id = ?`,
		env["event_id"]).Scan(&storedAt); err != nil {
		t.Fatalf("read back the stored row: %v", err)
	}
	if receipt.IngestedAtMs != storedAt {
		t.Errorf("the receipt's ingested_at_ms = %d, but the stored row holds %d — "+
			"the receipt must be read back from the stored row", receipt.IngestedAtMs, storedAt)
	}
}

// TestEventIngest_StrictDuplicateIsOneRowAndReportsDuplicate pins §3.5's
// "at most one stored row per event_id" and §6.4's read-back
// determination.
//
// The duplicate's receipt carries the ORIGINAL ingestion timestamp, not
// the timestamp of the second delivery.
func TestEventIngest_StrictDuplicateIsOneRowAndReportsDuplicate(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()

	first := lifecycleIngest(t, dm, env)
	second := lifecycleIngest(t, dm, env)

	if got := lifecycleRowCount(t, dm); got != 1 {
		t.Fatalf("stored rows = %d, want exactly 1 — the event_id is the dedup key", got)
	}
	if !second.Duplicate {
		t.Error("the second ingest must report duplicate: true")
	}
	if first.Duplicate {
		t.Error("the first ingest must not report duplicate: true")
	}
	if second.IngestedAtMs != first.IngestedAtMs {
		t.Errorf("the duplicate's ingested_at_ms = %d, want the ORIGINAL %d — "+
			"the receipt must be read back from the stored row",
			second.IngestedAtMs, first.IngestedAtMs)
	}
	if second.EventID != first.EventID {
		t.Errorf("the duplicate's event_id = %q, want %q", second.EventID, first.EventID)
	}
}

// TestEventIngest_DuplicateReceiptReadsBackTheOriginalTimestamp pins
// §6.4's read-back requirement DETERMINISTICALLY.
//
// Two ingests in a row are not enough to prove it: if both land in the
// same millisecond, the attempted timestamp and the stored timestamp
// coincide, and an implementation that simply reported what it *tried* to
// write would look identical. That is a real hole, not a theoretical one.
//
// So the pre-existing row is SEEDED with a sentinel timestamp. The
// receipt must carry the sentinel — the value the row holds — and never
// the value this call attempted to write. No sleep, no timing
// assumption: the two values are different by construction.
func TestEventIngest_DuplicateReceiptReadsBackTheOriginalTimestamp(t *testing.T) {
	dm := NewTestDM(t)

	const sentinelAtMs = int64(1)
	env := lifecycleEnvelope()
	eventID := env["event_id"].(string)

	// Seed the row the way a PREVIOUS ingestion would have left it.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO lifecycle_events (
			event_id, idempotency, schema_version, event_type, observation, trust,
			observed_at_ms, ingested_at_ms, host, source, adapter_version,
			project_id_source, framework_name, operation, result_status, meta
		) VALUES (?, 'strict', ?, 'session.observed', 'observed', 'observation',
		          1793021872123, ?, 'opencode', 'opencode-plugin', 'abc1234',
		          'unresolved', 'opencode', 'absent', 'unknown', '{}')`,
		eventID, LifecycleEventSchemaVersion, sentinelAtMs,
	); err != nil {
		t.Fatalf("seed the pre-existing row: %v", err)
	}

	receipt := lifecycleIngest(t, dm, env)

	if !receipt.Duplicate {
		t.Error("ingesting over a pre-existing event_id must report duplicate: true")
	}
	if receipt.IngestedAtMs != sentinelAtMs {
		t.Errorf("the receipt's ingested_at_ms = %d, want the STORED %d — the receipt "+
			"must be read back from the stored row, not from what this call attempted to write",
			receipt.IngestedAtMs, sentinelAtMs)
	}
	if receipt.EventID != eventID {
		t.Errorf("the receipt's event_id = %q, want %q", receipt.EventID, eventID)
	}
	if got := lifecycleRowCount(t, dm); got != 1 {
		t.Errorf("stored rows = %d, want 1", got)
	}
}

// TestEventIngest_BestEffortIdsAreNeverDeduplicated pins §3.5: in
// best-effort mode every emission is a distinct row.
func TestEventIngest_BestEffortIdsAreNeverDeduplicated(t *testing.T) {
	dm := NewTestDM(t)

	mk := func(id string) map[string]interface{} {
		env := lifecycleEnvelope()
		env["idempotency"] = "best_effort"
		env["event_id"] = id
		return env
	}
	lifecycleIngest(t, dm, mk("ev1b_"+strings.Repeat("c", 32)))
	lifecycleIngest(t, dm, mk("ev1b_"+strings.Repeat("d", 32)))

	if got := lifecycleRowCount(t, dm); got != 2 {
		t.Fatalf("stored rows = %d, want 2 — distinct best-effort ids are distinct rows", got)
	}

	// The same best-effort id twice is still one row: the KEY is the id,
	// whatever the mode.
	lifecycleIngest(t, dm, mk("ev1b_"+strings.Repeat("c", 32)))
	if got := lifecycleRowCount(t, dm); got != 2 {
		t.Fatalf("stored rows = %d, want 2 — the event_id is the key in either mode", got)
	}
}

// TestEventIngest_RejectsUnknownVersion pins the version authority: the
// ingest REJECTS an unrecognised version rather than ignoring it. A
// silent "ok" is how a caller comes to believe an observation is durable
// when nothing stored it.
func TestEventIngest_RejectsUnknownVersion(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()
	env["schema_version"] = "mpm-event/0.9"

	_, err := IngestLifecycleEvent(dm.SQLDB(), env)
	rej := lifecycleRejectionReason(t, err, LifecycleRejectUnknownVersion)
	if !strings.Contains(rej.Detail, "mpm-event/0.1") {
		t.Errorf("the rejection must name the supported version, got %q", rej.Detail)
	}
	if got := lifecycleRowCount(t, dm); got != 0 {
		t.Errorf("stored rows = %d, want 0 — a rejected event must not be stored", got)
	}
}

// TestEventIngest_RejectsUnknownEventType pins §3.9 rule 6: an unknown
// event type is rejected, NEVER mapped onto a neighbouring type.
func TestEventIngest_RejectsUnknownEventType(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()
	env["event_type"] = "session.resumed"

	_, err := IngestLifecycleEvent(dm.SQLDB(), env)
	lifecycleRejectionReason(t, err, LifecycleRejectUnknownEventType)
	if got := lifecycleRowCount(t, dm); got != 0 {
		t.Errorf("stored rows = %d, want 0", got)
	}
}

// TestEventIngest_RejectsNonNullPayload pins §3.8: payload is reserved
// and always null in 0.1.
func TestEventIngest_RejectsNonNullPayload(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()
	env["payload"] = map[string]interface{}{"content": "the assistant's reply"}

	_, err := IngestLifecycleEvent(dm.SQLDB(), env)
	lifecycleRejectionReason(t, err, LifecycleRejectInvalidEnvelope)
	if got := lifecycleRowCount(t, dm); got != 0 {
		t.Errorf("stored rows = %d, want 0", got)
	}
}

// TestEventIngest_RejectsAdapterSetIngestedAtMs pins §3.2: an adapter
// that sets ingested_at_ms is malformed.
func TestEventIngest_RejectsAdapterSetIngestedAtMs(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()
	env["ingested_at_ms"] = int64(1793021872456)

	_, err := IngestLifecycleEvent(dm.SQLDB(), env)
	lifecycleRejectionReason(t, err, LifecycleRejectInvalidEnvelope)
}

// TestEventIngest_RejectsUnknownEnvelopeField pins §3.1: the field set
// is FROZEN on acceptance. An unknown field is not an extension; it is a
// malformed envelope.
func TestEventIngest_RejectsUnknownEnvelopeField(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()
	env["secret_field"] = "surprise"

	_, err := IngestLifecycleEvent(dm.SQLDB(), env)
	lifecycleRejectionReason(t, err, LifecycleRejectInvalidEnvelope)
}

// TestEventIngest_RejectsOverLimitEnvelope pins §3.8 and §3.9 rule 8: an
// over-limit event is REJECTED WHOLE. It is never truncated into a
// valid-looking event.
func TestEventIngest_RejectsOverLimitEnvelope(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()
	env["adapter_version"] = strings.Repeat("x", 600)

	_, err := IngestLifecycleEvent(dm.SQLDB(), env)
	lifecycleRejectionReason(t, err, LifecycleRejectLimitExceeded)
	if got := lifecycleRowCount(t, dm); got != 0 {
		t.Errorf("stored rows = %d, want 0 — an over-limit event must not be truncated into a valid one", got)
	}
}

// TestEventIngest_RejectsOverLimitMeta pins the meta bounds.
func TestEventIngest_RejectsOverLimitMeta(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()
	env["meta"] = map[string]interface{}{
		"host_call_id": strings.Repeat("y", 200), // the bound is 128
	}
	_, err := IngestLifecycleEvent(dm.SQLDB(), env)
	lifecycleRejectionReason(t, err, LifecycleRejectLimitExceeded)

	// Too many meta fields.
	env2 := lifecycleEnvelope()
	meta := map[string]interface{}{}
	for i := 0; i < lifecycleMetaFieldsMax+1; i++ {
		meta[fmt.Sprintf("host_result_%d", i)] = "x"
	}
	env2["meta"] = meta
	_, err = IngestLifecycleEvent(dm.SQLDB(), env2)
	lifecycleRejectionReason(t, err, LifecycleRejectLimitExceeded)
}

// TestEventIngest_RejectsUnknownMetaKey pins that the allowlist is
// ENFORCED at the boundary, not merely documented. A key that is not on
// the allowlist is a field that was not permitted to cross.
func TestEventIngest_RejectsUnknownMetaKey(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()
	env["meta"] = map[string]interface{}{
		"host_event_type": "session.text.delta",
		"the_prompt":      "PROMPT_SECRET_alice_private_thing",
	}

	_, err := IngestLifecycleEvent(dm.SQLDB(), env)
	rej := lifecycleRejectionReason(t, err, LifecycleRejectInvalidEnvelope)
	if strings.Contains(rej.Detail, "PROMPT_SECRET") {
		t.Error("the rejection message must not carry the offending value")
	}
	if got := lifecycleRowCount(t, dm); got != 0 {
		t.Errorf("stored rows = %d, want 0", got)
	}
}

// TestEventIngest_RejectsIncoherentOutcome pins §3.7's outcome rules.
func TestEventIngest_RejectsIncoherentOutcome(t *testing.T) {
	dm := NewTestDM(t)

	cases := []struct {
		name string
		mut  func(map[string]interface{})
	}{
		{"operation absent but a success claimed", func(e map[string]interface{}) {
			e["result_status"] = "success"
		}},
		{"operation absent but an outcome class claimed", func(e map[string]interface{}) {
			e["outcome_class"] = "ok"
		}},
		{"operation present but the status is unknown", func(e map[string]interface{}) {
			e["operation"] = "present"
			e["outcome_class"] = "ok"
			e["result_status"] = "unknown"
		}},
		{"operation present without an outcome class", func(e map[string]interface{}) {
			e["operation"] = "present"
			e["result_status"] = "success"
		}},
		{"outcome class outside the enum", func(e map[string]interface{}) {
			e["operation"] = "present"
			e["result_status"] = "success"
			e["outcome_class"] = "brilliant"
		}},
		{"outcome code present with an ok class", func(e map[string]interface{}) {
			e["operation"] = "present"
			e["result_status"] = "success"
			e["outcome_class"] = "ok"
			e["outcome_code"] = "nope"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := lifecycleEnvelope()
			tc.mut(env)
			_, err := IngestLifecycleEvent(dm.SQLDB(), env)
			lifecycleRejectionReason(t, err, LifecycleRejectInvalidEnvelope)
		})
	}
	if got := lifecycleRowCount(t, dm); got != 0 {
		t.Errorf("stored rows = %d, want 0", got)
	}
}

// TestEventIngest_RejectsNonObservationTrust pins §3.7: trust is
// 'observation' and ONLY 'observation'. A trusted artifact is a memory, a
// lesson or a decision, not an event.
func TestEventIngest_RejectsNonObservationTrust(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()
	env["trust"] = "trusted"

	_, err := IngestLifecycleEvent(dm.SQLDB(), env)
	lifecycleRejectionReason(t, err, LifecycleRejectInvalidEnvelope)
}

// TestEventIngest_RejectsHostCallIDAsIdentity pins the core side of
// amendment A1: the host's per-call correlation id is NEVER an event
// identity. Two events that differ only in host_call_id are two rows.
func TestEventIngest_RejectsHostCallIDAsIdentity(t *testing.T) {
	dm := NewTestDM(t)

	envA := lifecycleEnvelope()
	envA["event_id"] = "ev1_" + strings.Repeat("1", 64)
	envA["meta"] = map[string]interface{}{"host_call_id": "call_shared"}

	envB := lifecycleEnvelope()
	envB["event_id"] = "ev1_" + strings.Repeat("2", 64)
	envB["meta"] = map[string]interface{}{"host_call_id": "call_shared"}

	lifecycleIngest(t, dm, envA)
	lifecycleIngest(t, dm, envB)

	if got := lifecycleRowCount(t, dm); got != 2 {
		t.Fatalf("stored rows = %d, want 2 — a shared host_call_id must NOT collapse "+
			"two distinct events into one", got)
	}

	// And the shared correlation id is NOT an independence signal: the
	// counters must not treat the two rows as two independent
	// corroborations of anything. There is no such counter, and the
	// stored count is simply the number of events.
	var shared int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lifecycle_events WHERE meta LIKE '%call_shared%'`).Scan(&shared); err != nil {
		t.Fatalf("count the correlated rows: %v", err)
	}
	if shared != 2 {
		t.Errorf("correlated rows = %d, want 2", shared)
	}
}

// TestEventIngest_CapturesNoKnowledgeArtifact pins §3.9 rule 2: capturing
// an observation creates NO memory, no lesson, no decision and no theory.
func TestEventIngest_CapturesNoKnowledgeArtifact(t *testing.T) {
	dm := NewTestDM(t)

	before := map[string]int{}
	for _, table := range []string{"memories", "lessons_base", "evidence"} {
		var n int
		if err := dm.SQLDB().QueryRow(
			fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		before[table] = n
	}

	lifecycleIngest(t, dm, lifecycleEnvelope())
	lifecycleIngest(t, dm, lifecycleToolEnvelope())

	for table, want := range before {
		var got int
		if err := dm.SQLDB().QueryRow(
			fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != want {
			t.Errorf("%s grew from %d to %d — capturing an observation must create no knowledge artifact",
				table, want, got)
		}
	}
}

// TestEventIngest_RejectionIsClassifiedWithATypedOutcomeCode pins the
// machine-readable reason: a rejection classifies as a validation outcome
// with the contract's reason token, so the counters can group by it
// without parsing prose.
func TestEventIngest_RejectionIsClassifiedWithATypedOutcomeCode(t *testing.T) {
	env := lifecycleEnvelope()
	env["event_type"] = "not.a.type"

	_, err := IngestLifecycleEvent(nil, env)
	// A nil database is rejected before the envelope is even read; use a
	// real one for the classification check.
	dm := NewTestDM(t)
	_, err = IngestLifecycleEvent(dm.SQLDB(), env)
	if err == nil {
		t.Fatal("expected a rejection")
	}
	class, code := ClassifyError(err)
	if class != OutcomeClassValidation {
		t.Errorf("outcome_class = %q, want %q — a rejected envelope is policy enforcement, not a substrate fault",
			class, OutcomeClassValidation)
	}
	if code != LifecycleRejectUnknownEventType {
		t.Errorf("outcome_code = %q, want %q", code, LifecycleRejectUnknownEventType)
	}
}

// TestEventIngest_MetaAllowlistMatchesTheContract pins that the core's
// allowlist is exactly the contract's §3.8 table — no more, no less.
func TestEventIngest_MetaAllowlistMatchesTheContract(t *testing.T) {
	want := []string{
		"changed_paths_count", "compaction_kind", "diff_stat_summary",
		"duration_ms", "host_action", "host_agent", "host_call_id",
		"host_error_code", "host_event_type", "host_message_id",
		"host_message_kind", "host_model_id", "host_model_provider",
		"host_result", "host_tool_name", "overflow", "payload_hash",
	}
	got := LifecycleEventMetaKeys()
	if len(got) != len(want) {
		t.Fatalf("the allowlist has %d keys, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("the allowlist's key %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// ── The counters ─────────────────────────────────────────────────────

// TestEventCounters_ReportStoredEventsByType pins that the counters
// match the rows.
func TestEventCounters_ReportStoredEventsByType(t *testing.T) {
	dm := NewTestDM(t)

	lifecycleIngest(t, dm, lifecycleEnvelope())
	lifecycleIngest(t, dm, lifecycleToolEnvelope())

	counters, err := LifecycleEventCounters(dm.SQLDB())
	if err != nil {
		t.Fatalf("LifecycleEventCounters: %v", err)
	}
	if counters["stored_total"].(int64) != 2 {
		t.Errorf("stored_total = %v, want 2", counters["stored_total"])
	}
	byType := counters["stored_by_event_type"].(map[string]int64)
	if byType["session.observed"] != 1 || byType["tool.outcome"] != 1 {
		t.Errorf("stored_by_event_type = %v", byType)
	}
	byVersion := counters["stored_by_schema_version"].(map[string]int64)
	if byVersion[LifecycleEventSchemaVersion] != 2 {
		t.Errorf("stored_by_schema_version = %v", byVersion)
	}
}

// TestEventCounters_DuplicatesAreNotCounted pins that a duplicate does
// not inflate the stored count.
func TestEventCounters_DuplicatesAreNotCounted(t *testing.T) {
	dm := NewTestDM(t)
	env := lifecycleEnvelope()

	lifecycleIngest(t, dm, env)
	lifecycleIngest(t, dm, env)

	counters, err := LifecycleEventCounters(dm.SQLDB())
	if err != nil {
		t.Fatalf("LifecycleEventCounters: %v", err)
	}
	if counters["stored_total"].(int64) != 1 {
		t.Errorf("stored_total = %v, want 1 — a duplicate must not inflate the count",
			counters["stored_total"])
	}
}

// TestEventCounters_AreNotPersistedSeparately pins that there is NO
// counter table: the counters are computed on read from authoritative
// state. A persisted counter would be a second source of truth that could
// drift from the rows.
func TestEventCounters_AreNotPersistedSeparately(t *testing.T) {
	dm := NewTestDM(t)

	rows, err := dm.SQLDB().Query(
		`SELECT name FROM sqlite_master WHERE type='table' AND (name LIKE '%counter%' OR name LIKE '%lifecycle%')`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name != "lifecycle_events" {
			t.Errorf("the store must not have a %q table — the counters are computed on read", name)
		}
	}
}

// ── The ordering key (§3.6) ──────────────────────────────────────────

// TestEventIngest_OrdersByTheContractKey pins §3.6's total, deterministic
// order, including the rule that a NULL host_seq sorts AFTER every
// non-NULL host_seq at the same observed_at_ms.
func TestEventIngest_OrdersByTheContractKey(t *testing.T) {
	dm := NewTestDM(t)

	mk := func(id string, observedAt int64, seq interface{}) {
		env := lifecycleEnvelope()
		env["event_id"] = id
		env["observed_at_ms"] = observedAt
		if seq != nil {
			env["host_seq"] = seq
		}
		lifecycleIngest(t, dm, env)
	}
	base := int64(1793021872123)
	mk("ev1_"+strings.Repeat("3", 64), base, nil)      // NULL seq
	mk("ev1_"+strings.Repeat("4", 64), base, int64(2)) // seq 2
	mk("ev1_"+strings.Repeat("5", 64), base, int64(1)) // seq 1
	mk("ev1_"+strings.Repeat("6", 64), base-1, nil)    // earlier

	rows, err := dm.SQLDB().Query(`
		SELECT event_id FROM lifecycle_events
		ORDER BY observed_at_ms ASC, (host_seq IS NULL) ASC, host_seq ASC, event_id ASC`)
	if err != nil {
		t.Fatalf("order query: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}

	want := []string{
		"ev1_" + strings.Repeat("6", 64), // earlier observed_at
		"ev1_" + strings.Repeat("5", 64), // seq 1
		"ev1_" + strings.Repeat("4", 64), // seq 2
		"ev1_" + strings.Repeat("3", 64), // NULL seq sorts LAST
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %s, want %s (full order: %v)", i, got[i], want[i], got)
		}
	}
}

// TestEventIngest_OrderIndexIsUsed pins that the ordering index matches
// the ordering expression, so the read is a covering-index scan rather
// than a sort.
func TestEventIngest_OrderIndexIsUsed(t *testing.T) {
	dm := NewTestDM(t)

	rows, err := dm.SQLDB().Query(`
		EXPLAIN QUERY PLAN
		SELECT event_id FROM lifecycle_events
		ORDER BY observed_at_ms ASC, (host_seq IS NULL) ASC, host_seq ASC, event_id ASC`)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, " | ")
	if !strings.Contains(joined, "idx_lifecycle_events_order") {
		t.Errorf("the ordering read must use idx_lifecycle_events_order, plan was: %s", joined)
	}
	if strings.Contains(joined, "TEMP B-TREE") {
		t.Errorf("the ordering read must not sort, plan was: %s", joined)
	}
}
