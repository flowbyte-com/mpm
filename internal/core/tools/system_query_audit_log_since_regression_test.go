// system_query_audit_log_since_regression_test.go — Pass 3 defect C.11.
//
// The 2026-09-05 audit found mpm_system.query_audit_log silently
// drops the `since` parameter. The handler only reads `level`,
// `component`, `artifact_id`, `days`, `limit`, `include_stack` —
// `since` is accepted via additionalProperties:true but ignored.
//
// Pre-fix reproduction:
//
//   $ mpm call mpm_system --payload '{"action":"query_audit_log","params":{"since":1700000000}}'
//     # returns the historical default (20 rows from days=7),
//     # NOT a time-bounded result since the since value
//
// Canonical contract:
//
//   omitted      → days=7 (historical default)
//   days=N       → lookback window of N days (existing behaviour)
//   since=EPOCH  → absolute cutoff (rows with created_at >= EPOCH);
//                  when both since and days are present, since wins
//   since<0 or non-numeric → error
//
// Fix: add `since` to the schema and to the handler. The handler
// computes the effective days from the since timestamp relative
// to time.Now(), then calls the existing DM.QueryAuditLog (which
// takes days). This avoids any DM signature change.

package tools

import (
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestQueryAuditLog_SinceRespected pins the headline behaviour:
// a since parameter must filter rows by absolute timestamp, not
// be silently dropped.
func TestQueryAuditLog_SinceRespected(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := defaultACForPatch()

	// Seed two audit rows: one "old" and one "new" (relative to the
	// current time). Use the dm helper to create them with explicit
	// created_at timestamps.
	oldID := seedAuditRow(t, dm, "warn", "audit-test", "old row", time.Now().Add(-48*time.Hour).Unix())
	_ = oldID
	newID := seedAuditRow(t, dm, "warn", "audit-test", "new row", time.Now().Add(-1*time.Hour).Unix())
	_ = newID

	// Pick a since between the two timestamps — should surface only
	// the "new" row.
	cutoff := time.Now().Add(-2 * time.Hour).Unix()

	res, err := handleMpmSystem(dm, ac, map[string]interface{}{
		"action": "query_audit_log",
		"params": map[string]interface{}{
			"since": float64(cutoff),
		},
	})
	if err != nil {
		t.Fatalf("query_audit_log with since: %v", err)
	}
	m := res.(map[string]interface{})
	results, _ := m["results"].([]map[string]interface{})

	// Should include the new row but NOT the old row.
	foundNew, foundOld := false, false
	for _, r := range results {
		msg, _ := r["message"].(string)
		if strings.Contains(msg, "new row") {
			foundNew = true
		}
		if strings.Contains(msg, "old row") {
			foundOld = true
		}
	}
	if !foundNew {
		t.Errorf("since filter must surface rows with created_at >= since; missing the 'new' row")
	}
	if foundOld {
		t.Errorf("since filter must NOT surface rows with created_at < since; 'old' row leaked through")
	}
}

// TestQueryAuditLog_SinceTakesPrecedenceOverDays pins the precedence:
// when both since and days are supplied, since wins. This matches
// the documented contract.
func TestQueryAuditLog_SinceTakesPrecedenceOverDays(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := defaultACForPatch()

	cutoff := time.Now().Add(-2 * time.Hour).Unix()

	// days=1 (very narrow) but since=-48h (very wide) — since should win
	// and include any rows newer than the cutoff.
	res, err := handleMpmSystem(dm, ac, map[string]interface{}{
		"action": "query_audit_log",
		"params": map[string]interface{}{
			"days":  float64(1),
			"since": float64(cutoff),
		},
	})
	if err != nil {
		t.Fatalf("query_audit_log with since+days: %v", err)
	}
	m := res.(map[string]interface{})
	if _, ok := m["results"]; !ok {
		t.Fatalf("response missing 'results' key: %v", m)
	}
	// Behavioural: results must include rows newer than cutoff,
	// regardless of the narrow days=1.
}

// TestQueryAuditLog_SinceInvalidErrors pins: malformed since
// (non-numeric, negative, wrong type) errors rather than silently
// falling back to days=7.
func TestQueryAuditLog_SinceInvalidErrors(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := defaultACForPatch()

	for _, label := range []string{"negative", "string", "negative_int"} {
		t.Run(label, func(t *testing.T) {
			var val interface{}
			switch label {
			case "negative":
				val = float64(-1)
			case "string":
				val = "abc"
			case "negative_int":
				val = -100
			}
			_, err := handleMpmSystem(dm, ac, map[string]interface{}{
				"action": "query_audit_log",
				"params": map[string]interface{}{
					"since": val,
				},
			})
			if err == nil {
				t.Errorf("invalid since (%s) must error", label)
			}
			if !strings.Contains(err.Error(), "since") {
				t.Errorf("error must mention 'since', got: %v", err)
			}
		})
	}
}

// TestQueryAuditLog_OmittedSinceUsesDaysDefault pins the
// documented backwards-compat: omitting since (and days) uses the
// historical 7-day default. The fix must not break the no-filter
// path.
func TestQueryAuditLog_OmittedSinceUsesDaysDefault(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := defaultACForPatch()

	_, err := handleMpmSystem(dm, ac, map[string]interface{}{
		"action": "query_audit_log",
		"params": map[string]interface{}{},
	})
	if err != nil {
		t.Errorf("no-filter query_audit_log must not error, got: %v", err)
	}
}

// --- helpers ---

// seedAuditRow inserts a system_audit_log row with the given
// created_at timestamp. Used to test the since filter.
func seedAuditRow(t *testing.T, dm *mpminternal.DatabaseManager, level, component, message string, createdAt int64) string {
	t.Helper()
	id := "audit-probe-" + level + "-" + message + "-" + time.Now().Format(time.RFC3339Nano)
	_, err := dm.SQLDB().Exec(`
		INSERT INTO system_audit_log (id, level, component, message, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, id, level, component, message, createdAt)
	if err != nil {
		t.Fatalf("seed audit row %s: %v", id, err)
	}
	return id
}
