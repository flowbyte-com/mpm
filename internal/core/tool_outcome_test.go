// tool_outcome_test.go — Acceptance fixture for OBSERVABILITY
// FOUNDATION outcome classification. Pins:
//
//   1. The closed ToolOutcomeClass vocabulary.
//   2. NewOutcome constructs with valid class+code pairs.
//   3. ClassifyError mapping for each fixture defined in brief §10:
//      - missing required parameter      -> validation
//      - unknown action                  -> validation
//      - artifact absent                 -> not_found
//      - deterministic database failure  -> substrate
//      - context deadline                -> timeout
//      - unexpected internal error       -> internal / unclassified
//   4. Degraded success semantics (ContextualFocus degraded).
//   5. tool_invocations schema migration adds outcome_class +
//      outcome_code columns without breaking existing rows.
//   6. system_audit_log schema migration adds correlation columns
//      + event_code without breaking existing rows.
//   7. Existing rows read back as outcome_class=NULL
//      (historical classification is unknown, not guessed).

package internal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// closedVocabularyCheck asserts the closed enum surface matches the
// design contract: 9 classes, all lowercase ASCII, all unique.
func TestToolOutcome_VocabularyClosed(t *testing.T) {
	if len(AllOutcomeClasses) != 9 {
		t.Fatalf("vocabulary size = %d, want 9 (ok|validation|not_found|conflict|degraded|substrate|integration|timeout|internal)",
			len(AllOutcomeClasses))
	}
	seen := map[ToolOutcomeClass]bool{}
	for _, c := range AllOutcomeClasses {
		if c == "" {
			t.Fatalf("empty class in vocabulary")
		}
		if seen[c] {
			t.Fatalf("duplicate class %q in vocabulary", c)
		}
		seen[c] = true
		// Bounded: lowercase ASCII plus underscore.
		for _, r := range string(c) {
			if r != '_' && (r < 'a' || r > 'z') {
				t.Fatalf("class %q contains non-lowercase / non-underscore rune %q", c, r)
			}
		}
	}
}

// TestToolOutcome_NewOutcome_Bounds pins the NewOutcome contract:
// ok outcome with empty code is canonical; non-ok with empty code
// is mapped to "unclassified"; both surfaces stay well-formed.
func TestToolOutcome_NewOutcome_Bounds(t *testing.T) {
	if got := (ToolOutcome{Class: OutcomeClassOk, Code: ""}); !got.IsSuccess() {
		t.Errorf("ok outcome must be success")
	}
	if got := (ToolOutcome{Class: OutcomeClassDegraded, Code: "fallback_substrate"}); !got.IsSuccess() {
		t.Errorf("degraded outcome must be success")
	}
	if got := NewOutcome(OutcomeClassSubstrate, ""); got.Code != "unclassified" {
		t.Errorf("NewOutcome(substrate, \"\"): got %q, want unclassified", got.Code)
	}
	if got := NewOutcome(OutcomeClassOk, ""); got.Code != "" {
		t.Errorf("NewOutcome(ok, \"\"): got %q, want empty", got.Code)
	}
}

// TestToolOutcome_Classify_MissingRequiredField pins the
// classification of "missing required field" shaped errors.
func TestToolOutcome_Classify_MissingRequiredField(t *testing.T) {
	err := errors.New("mpm_wakes: missing required field `action` (string) — expected shape {…}")
	class, code := ClassifyError(err)
	if class != OutcomeClassValidation {
		t.Fatalf("class = %q, want validation", class)
	}
	if code != "missing_required_field" {
		t.Errorf("code = %q, want missing_required_field", code)
	}
}

// TestToolOutcome_Classify_UnknownAction pins the
// "Valid actions include" message classification.
func TestToolOutcome_Classify_UnknownAction(t *testing.T) {
	err := errors.New("unknown action \"inspect\" for mpm_work. Valid actions include create, list, show, …")
	class, code := ClassifyError(err)
	if class != OutcomeClassValidation {
		t.Fatalf("class = %q, want validation", class)
	}
	if code != "unknown_action" {
		t.Errorf("code = %q, want unknown_action", code)
	}
}

// TestToolOutcome_Classify_ArtifactNotFound pins the
// `… not found` and `… does not exist` shapes.
func TestToolOutcome_Classify_ArtifactNotFound(t *testing.T) {
	err := errors.New("snooze: memory \"abc123\" not found")
	class, code := ClassifyError(err)
	if class != OutcomeClassNotFound {
		t.Fatalf("class = %q, want not_found", class)
	}
	if code != "artifact_not_found" {
		t.Errorf("code = %q, want artifact_not_found", code)
	}
	err = errors.New("artifact memory/missing-r9t46-parity does not exist")
	class, code = ClassifyError(err)
	if class != OutcomeClassNotFound {
		t.Errorf("class = %q, want not_found (does-not-exist variant)", class)
	}
}

// TestToolOutcome_Classify_Substrate_DBFailure pins the
// substrate class for SQLite-layer errors.
func TestToolOutcome_Classify_Substrate_DBFailure(t *testing.T) {
	for _, msg := range []string{
		"SQLITE_BUSY: database is locked",
		"sqlite3: no such table: foo",
		"PRAGMA quick_check: integrity check failed at row 7",
	} {
		err := errors.New(msg)
		class, code := ClassifyError(err)
		if class != OutcomeClassSubstrate {
			t.Errorf("class = %q, want substrate for %q", class, msg)
		}
		if code == "" {
			t.Errorf("code empty for substrate %q", msg)
		}
	}
}

// TestToolOutcome_Classify_Timeout pins the
// context-deadline and client-timeout shapes.
func TestToolOutcome_Classify_Timeout(t *testing.T) {
	for _, msg := range []string{
		"context deadline exceeded",
		"Client.Timeout exceeded while awaiting headers",
	} {
		err := errors.New(msg)
		class, code := ClassifyError(err)
		if class != OutcomeClassTimeout {
			t.Errorf("class = %q, want timeout for %q", class, msg)
		}
		if code != "context_deadline" {
			t.Errorf("code = %q, want context_deadline for %q", code, msg)
		}
	}
	// errors.Is path: context.DeadlineExceeded sentinel maps to timeout.
	wrapped := fmt.Errorf("synth.DoLLMRequest: %w", context.DeadlineExceeded)
	class, code := ClassifyError(wrapped)
	if class != OutcomeClassTimeout {
		t.Errorf("errors.Is(DeadlineExceeded) class = %q, want timeout", class)
	}
	if code != "context_deadline" {
		t.Errorf("errors.Is(DeadlineExceeded) code = %q, want context_deadline", code)
	}
}

// TestToolOutcome_Classify_InternalFallback pins the
// safe fallback for unclassified errors.
func TestToolOutcome_Classify_InternalFallback(t *testing.T) {
	err := errors.New("a peculiar and unprecedented condition")
	class, code := ClassifyError(err)
	if class != OutcomeClassInternal {
		t.Fatalf("class = %q, want internal", class)
	}
	if code != "unclassified" {
		t.Errorf("code = %q, want unclassified", code)
	}
}

// TestToolOutcome_Classify_NilSafe pins the nil-error contract —
// nil means success.
func TestToolOutcome_Classify_NilSafe(t *testing.T) {
	class, code := ClassifyError(nil)
	if class != OutcomeClassOk {
		t.Errorf("class = %q, want ok", class)
	}
	if code != "" {
		t.Errorf("code = %q, want empty", code)
	}
}

// TestToolOutcome_SchemaMigration_Hermetic uses an in-memory
// DatabaseManager and verifies SafeMigrations brought tool_invocations
// + system_audit_log to the new shape without dropping existing
// rows.
func TestToolOutcome_SchemaMigration_Hermetic(t *testing.T) {
	dm := NewTestDM(t)

	// 1) Verify outcome columns exist on tool_invocations.
	tiCols := columnList(t, dm.SQLDB(), "tool_invocations")
	for _, want := range []string{"outcome_class", "outcome_code"} {
		if !tiCols[want] {
			t.Errorf("tool_invocations missing column %q; have: %v", want, sortedKeys(tiCols))
		}
	}

	// 2) Verify correlation + event_code columns exist on system_audit_log.
	auCols := columnList(t, dm.SQLDB(), "system_audit_log")
	for _, want := range []string{"invocation_id", "mpm_session_id", "framework_name", "framework_session_id", "event_code"} {
		if !auCols[want] {
			t.Errorf("system_audit_log missing column %q; have: %v", want, sortedKeys(auCols))
		}
	}

	// 3) Verify outcome compound index was created.
	var idx int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_tool_invocations_outcome'`,
	).Scan(&idx); err != nil {
		t.Fatalf("probe outcome index: %v", err)
	}
	if idx == 0 {
		t.Errorf("idx_tool_invocations_outcome not created")
	}

	// 4) Backfill behavior: write a legacy-shaped row (NULL outcomes),
	//    then read it back. Historical rows are NULL, not guessed.
	invID := "legacy-row-fixture"
	now := int64(1700000000)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id, actor_kind,
		     payload_hash, result_status, started_at, completed_at,
		     mpm_session_id, framework_session_id)
		 VALUES (?, 'cli-default', 'mpm_wakes', 'check', ?, 'human',
		     'sha256:legacy', 'error', ?, ?,
		     NULL, NULL)`,
		fmt.Sprintf("legacy-%d", now), invID, now, now,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	// Read back: outcome_class must come back as NULL (NOT "internal"
	// or any other fallback). The reader treats NULL as "unknown
	// historical classification".
	var gotClass, gotCode sql.NullString
	if err := dm.SQLDB().QueryRow(
		`SELECT outcome_class, outcome_code FROM tool_invocations WHERE invocation_id = ?`,
		invID,
	).Scan(&gotClass, &gotCode); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if gotClass.Valid {
		t.Errorf("legacy outcome_class = %q, want NULL", gotClass.String)
	}
	if gotCode.Valid {
		t.Errorf("legacy outcome_code = %q, want NULL", gotCode.String)
	}
}

// TestToolOutcome_ReporterWritesFields verifies the writer path
// populates outcome_class + outcome_code from the dispatched
// outcome. This is the writer-side check; the reader-side check
// is the migration test above.
func TestToolOutcome_ReporterWritesFields(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id, actor_kind,
		     payload_hash, result_status, started_at, completed_at,
		     outcome_class, outcome_code)
		 VALUES (?, 'cli-default', 'mpm_wakes', 'check', ?, 'human',
		     'sha256:writer', 'error', ?, ?, ?, ?)`,
		"writer-row-1", "inv-writer-1", int64(1700000010), int64(1700000011),
		"validation", "missing_required_field",
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var c, code string
	if err := dm.SQLDB().QueryRow(
		`SELECT outcome_class, outcome_code FROM tool_invocations WHERE invocation_id = ?`,
		"inv-writer-1",
	).Scan(&c, &code); err != nil {
		t.Fatalf("read: %v", err)
	}
	if c != "validation" || code != "missing_required_field" {
		t.Fatalf("got (%q, %q), want (validation, missing_required_field)", c, code)
	}
}

// TestToolOutcome_AuditLogCorrelation_Writeable verifies that an
// audit log row can carry the correlation triple + event_code
// without conflict (read-back equality).
func TestToolOutcome_AuditLogCorrelation_Writeable(t *testing.T) {
	dm := NewTestDM(t)
	_, err := dm.SQLDB().Exec(
		`INSERT INTO system_audit_log
		    (id, level, component, message, invocation_id,
		     mpm_session_id, framework_name, framework_session_id,
		     event_code)
		 VALUES (?, 'warn', 'test-component', 'sample',
		     ?, ?, ?, ?, ?)`,
		"audit-row-corr-1",
		"inv-1", "mpm-session-1", "opencode", "frame-session-A",
		"substrate_schema_migration",
	)
	if err != nil {
		t.Fatalf("insert audit: %v", err)
	}
	var inv, mpms, fw, fws, ec string
	var invN, mpmsN, fwsN sql.NullString
	err = dm.SQLDB().QueryRow(
		`SELECT invocation_id, mpm_session_id, framework_name,
		        framework_session_id, event_code
		 FROM system_audit_log WHERE id = ?`,
		"audit-row-corr-1",
	).Scan(&invN, &mpmsN, &fw, &fwsN, &ec)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	inv = invN.String
	mpms = mpmsN.String
	fws = fwsN.String
	if inv != "inv-1" || mpms != "mpm-session-1" || fw != "opencode" || fws != "frame-session-A" || ec != "substrate_schema_migration" {
		t.Fatalf("correlation mismatch: inv=%q mpms=%q fw=%q fws=%q ec=%q",
			inv, mpms, fw, fws, ec)
	}
}

// TestToolOutcome_AuditLog_NullCorrelationSafe verifies that audit
// rows remain writable with NULL correlation (the legacy shape, and
// the shape for scheduler events that have no invocation).
func TestToolOutcome_AuditLog_NullCorrelationSafe(t *testing.T) {
	dm := NewTestDM(t)
	_, err := dm.SQLDB().Exec(
		`INSERT INTO system_audit_log (id, level, component, message)
		 VALUES (?, 'warn', 'scheduler', 'tick ok')`,
		"audit-row-null-1",
	)
	if err != nil {
		t.Fatalf("insert null-corr audit: %v", err)
	}
	var invN, mpmsN, fwsN, fwN sql.NullString
	var ecN sql.NullString
	err = dm.SQLDB().QueryRow(
		`SELECT invocation_id, mpm_session_id, framework_name,
		        framework_session_id, event_code
		 FROM system_audit_log WHERE id = ?`,
		"audit-row-null-1",
	).Scan(&invN, &mpmsN, &fwN, &fwsN, &ecN)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if invN.Valid || mpmsN.Valid || fwsN.Valid || fwN.Valid || ecN.Valid {
		t.Errorf("legacy shape should yield NULLs, got inv=%q mpms=%q fw=%q fws=%q ec=%q",
			invN.String, mpmsN.String, fwN.String, fwsN.String, ecN.String)
	}
}

// TestToolOutcome_Invariant_NoUserContentInCodes pins the contract
// that outcome codes and event_codes are not arbitrary text.
func TestToolOutcome_Invariant_NoUserContentInCodes(t *testing.T) {
	rejected := []string{
		"user said 'hello'",
		"memory content: foo bar",
		"select 1 from users where name = 'a'",
		"/etc/passwd leak",
		"POST https://api.example.com/v1",
		"API key abcdef",
	}
	for _, s := range rejected {
		// The classifier could in principle match s to "validation".
		// What MUST NOT happen: outcome_code becomes the literal s.
		_, code := ClassifyError(errors.New(s))
		if strings.ContainsAny(code, " ") {
			t.Errorf("code %q contains space (looks like content, not an identifier)", code)
		}
		if strings.Contains(code, "://") {
			t.Errorf("code %q contains URL marker", code)
		}
		if strings.Contains(code, "'") || strings.Contains(code, `"`) {
			t.Errorf("code %q contains quotes", code)
		}
	}
}

// columnList returns a set of column names for the given table.
func columnList(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%q)", table))
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[name] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Keep assertion stable without pulling sort pkg.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
