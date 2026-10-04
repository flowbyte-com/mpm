package internal

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// ==================== Watchdog v2 ====================

// newTestDMWithWatchdog returns a hermetic DatabaseManager with watchdogPath
// rewired to a temp file. The same pattern as newTestDMWithMirror; kept
// separate so the two streams can be tested in isolation when the
// invariant is about one of them specifically.
func newTestDMWithWatchdog(t *testing.T) (*DatabaseManager, string) {
	t.Helper()
	dm := newTestDM(t)
	path := t.TempDir() + "/watchdog.jsonl"
	dm.watchdogPath = path
	return dm, path
}

// readWatchdogLines decodes every line of the watchdog log. The same
// tolerance policy as RecentWatchdogOps: a corrupt line is skipped, not
// fatal, so a tail truncation from a hard kill is not a test failure.
func readWatchdogLines(t *testing.T, path string) []map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if strings.Contains(err.Error(), "no such file") {
			return nil
		}
		t.Fatalf("read watchdog: %v", err)
	}
	out := []map[string]interface{}{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// watchdogSQLSentinels are the credential shapes the design requires to be
// absent from every watchdog record. Each is a real-world token family,
// not a synthetic string, because the point is to prove the SHAPE is
// stripped rather than to prove one literal is filtered.
var watchdogSQLSentinels = []struct {
	name  string
	value string
}{
	{"anthropic-key", "sk-ant-api03-" + strings.Repeat("AbCdEf0123", 3)},
	{"github-pat", "ghp_" + strings.Repeat("a1B2c3D4", 4) + "extra"},
	{"bearer-token", "Bearer abcdef0123456789"},
	{"private-key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY-----"},
}

// TestWatchdog_FastSuccessIsOmitted is the core v2 invariant: a statement
// that does what it is supposed to do, quickly, does not earn a record.
// 100 successful statements produce zero watchdog lines.
func TestWatchdog_FastSuccessIsOmitted(t *testing.T) {
	dm, path := newTestDMWithWatchdog(t)

	for i := 0; i < 100; i++ {
		dm.logSQLOutcome(watchdogOpExec, "SELECT 1", time.Millisecond, nil, 0)
	}
	if rows := readWatchdogLines(t, path); len(rows) != 0 {
		t.Errorf("100 fast successful statements produced %d watchdog lines, want 0", len(rows))
	}
}

// TestWatchdog_FailureIsRecorded pins the "error → one line at error level"
// half of the SQL outcome classifier. The error string is redacted as a
// defence-in-depth layer.
func TestWatchdog_FailureIsRecorded(t *testing.T) {
	dm, path := newTestDMWithWatchdog(t)

	dm.logSQLOutcome(watchdogOpExec, "INSERT INTO memories (x) VALUES (?)",
		2*time.Millisecond, errors.New("constraint failed: UNIQUE"), 0)

	rows := readWatchdogLines(t, path)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row["level"] != "error" {
		t.Errorf("level = %v, want error", row["level"])
	}
	if row["reason"] != "sql_error" {
		t.Errorf("reason = %v, want sql_error", row["reason"])
	}
	if row["op"] != "exec" {
		t.Errorf("op = %v, want exec", row["op"])
	}
	if shape, _ := row["sql_shape"].(string); strings.Contains(shape, "? VALUES") {
		t.Errorf("sql_shape did not reduce the parameter list to a ?: %q", shape)
	}
}

// TestWatchdog_BusyRetryIsRecorded pins the "retries > 0 → warn level"
// half. The record must declare how many retries the statement needed;
// that's the data an operator uses to recognise "the database is
// contended" rather than "this specific query is broken".
func TestWatchdog_BusyRetryIsRecorded(t *testing.T) {
	dm, path := newTestDMWithWatchdog(t)

	dm.logSQLOutcome(watchdogOpQuery, "SELECT * FROM memories", 30*time.Millisecond, nil, 3)

	rows := readWatchdogLines(t, path)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row["level"] != "warn" {
		t.Errorf("level = %v, want warn", row["level"])
	}
	if row["reason"] != "busy_retry" {
		t.Errorf("reason = %v, want busy_retry", row["reason"])
	}
	if retries, _ := row["retries"].(float64); retries != 3 {
		t.Errorf("retries = %v, want 3", row["retries"])
	}
}

// TestWatchdog_SlowStatementIsRecorded pins the "elapsed > threshold →
// warn" half. The record carries the elapsed time in milliseconds so an
// operator can histogram by operation.
func TestWatchdog_SlowStatementIsRecorded(t *testing.T) {
	dm, path := newTestDMWithWatchdog(t)

	dm.logSQLOutcome(watchdogOpQueryRow, "SELECT 1", 250*time.Millisecond, nil, 0)

	rows := readWatchdogLines(t, path)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row["level"] != "warn" {
		t.Errorf("level = %v, want warn", row["level"])
	}
	if row["reason"] != "slow" {
		t.Errorf("reason = %v, want slow", row["reason"])
	}
	if op, _ := row["op"].(string); op != "query_row" {
		t.Errorf("op = %v, want query_row", op)
	}
	if ms, _ := row["duration_ms"].(float64); ms < 200 {
		t.Errorf("duration_ms = %v, want >= 200", ms)
	}
}

// TestWatchdog_ErrorBeatsRetryBeatsSlow pins the precedence rule. A
// statement that is slow AND has retries AND fails produces exactly one
// line at error level, not three lines. Otherwise a single bad
// statement would triple-count and the log would be noise.
func TestWatchdog_ErrorBeatsRetryBeatsSlow(t *testing.T) {
	dm, path := newTestDMWithWatchdog(t)

	dm.logSQLOutcome(watchdogOpExec, "INSERT INTO x", 5*time.Second, errors.New("boom"), 4)

	rows := readWatchdogLines(t, path)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (error+retry+slow must collapse to one line)", len(rows))
	}
	if rows[0]["level"] != "error" {
		t.Errorf("level = %v, want error (highest severity wins)", rows[0]["level"])
	}
	if rows[0]["reason"] != "sql_error" {
		t.Errorf("reason = %v, want sql_error", rows[0]["reason"])
	}
}

// TestWatchdog_NeverLogsBoundValues is the design's SQL secrecy contract
// end-to-end: a Go-level argument that contains a credential must not
// appear in any watchdog record. The statement text is normalised, but
// the bound value is a Go argument and never appears in the statement
// text at all — this test pins that structural property.
func TestWatchdog_NeverLogsBoundValues(t *testing.T) {
	dm, path := newTestDMWithWatchdog(t)

	for _, s := range watchdogSQLSentinels {
		dm.logSQLOutcome(watchdogOpExec, "INSERT INTO x (v) VALUES (?)",
			2*time.Millisecond, errors.New("constraint failed: "+s.value), 0)
	}

	data, _ := os.ReadFile(path)
	body := string(data)
	for _, s := range watchdogSQLSentinels {
		if strings.Contains(body, strings.SplitN(s.value, "\n", 2)[0]) {
			t.Errorf("%s sentinel value present in watchdog log: %s", s.name, body)
		}
	}
}

// TestWatchdog_NeverLogsInterpolatedLiterals is the other half of the SQL
// secrecy contract: a credential interpolated into the statement text
// (the shape of a v1 SQL injection that the design exists to prevent)
// must be stripped by the normaliser.
func TestWatchdog_NeverLogsInterpolatedLiterals(t *testing.T) {
	dm, path := newTestDMWithWatchdog(t)

	for _, s := range watchdogSQLSentinels {
		stmt := "INSERT INTO x (v) VALUES ('" + s.value + "')"
		dm.logSQLOutcome(watchdogOpExec, stmt, 2*time.Millisecond,
			errors.New("constraint failed: "+s.value), 0)
	}

	data, _ := os.ReadFile(path)
	body := string(data)
	for _, s := range watchdogSQLSentinels {
		if strings.Contains(body, strings.SplitN(s.value, "\n", 2)[0]) {
			t.Errorf("%s sentinel value interpolated into SQL reached the watchdog log", s.name)
		}
	}
}

// TestWatchdog_SQLShapeBounded pins the 120-character cap on sql_shape. A
// shape that runs past a terminal line is a shape that won't be read.
func TestWatchdog_SQLShapeBounded(t *testing.T) {
	dm, path := newTestDMWithWatchdog(t)

	big := "SELECT " + strings.Repeat("col_a, col_b, col_c, ", 30) + "col_d FROM t WHERE x = 1"
	dm.logSQLOutcome(watchdogOpExec, big, 2*time.Millisecond,
		errors.New("boom"), 0)

	rows := readWatchdogLines(t, path)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	shape, _ := rows[0]["sql_shape"].(string)
	if n := len([]rune(shape)); n > sqlShapeMax {
		t.Errorf("sql_shape is %d chars, over %d", n, sqlShapeMax)
	}
	if !strings.HasSuffix(shape, "…") {
		t.Errorf("truncated sql_shape does not mark the cut: %q", shape)
	}
}

// TestWatchdog_SynthesizeFailedCarriesNoContent pins the synthesis
// lifecycle contract: a failed synthesis records id, reason and the
// driver's error. It does NOT carry a content excerpt — that was the v1
// behaviour the redesign removes.
func TestWatchdog_SynthesizeFailedCarriesNoContent(t *testing.T) {
	dm, path := newTestDMWithWatchdog(t)

	logSynthesizeEvent(dm, watchdogOpSynthesizeFailed, map[string]interface{}{
		"memory_id":        "mem-1",
		"safeguard_reason": "cooldown",
		"error":            "embedding timeout",
		// v1 also attached `content` — design removes it.
		"content": "Bearer abcdef0123456789",
	})

	rows := readWatchdogLines(t, path)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row["op"] != "synthesize_failed" {
		t.Errorf("op = %v, want synthesize_failed", row["op"])
	}
	if row["level"] != "error" {
		t.Errorf("level = %v, want error", row["level"])
	}
	if f, ok := row["fields"].(map[string]interface{}); ok {
		if _, present := f["content"]; present {
			t.Errorf("synthesize_failed record carried a content field; F-1 design removes this")
		}
		if v, _ := f["memory_id"].(string); v != "mem-1" {
			t.Errorf("memory_id lost: %v", f["memory_id"])
		}
	} else {
		t.Errorf("synthesize_failed record has no `fields` map: %v", row)
	}
	// And the credential, even if it slipped in via `error`, must be
	// redacted.
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "abcdef0123456789") {
		t.Errorf("bearer token survived redaction into the watchdog log")
	}
}

// TestWatchdog_DestructiveOperationIsAudited pins the watchdog
// "destruction of a log is a recordable event" rule. An operator who
// wipes the mirror should see a watchdog line that says so, in the one
// log that survives the wipe.
func TestWatchdog_DestructiveOperationIsAudited(t *testing.T) {
	dm, path := newTestDMWithWatchdog(t)

	dm.LogDestructiveOperation("memory wipe", 3)

	rows := readWatchdogLines(t, path)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row["op"] != "destructive_operation" {
		t.Errorf("op = %v, want destructive_operation", row["op"])
	}
	if f, _ := row["fields"].(map[string]interface{}); f == nil {
		t.Fatalf("destructive_operation record has no `fields` map: %v", row)
	} else {
		if v, _ := f["scope"].(string); v != "memory wipe" {
			t.Errorf("scope = %v, want 'memory wipe'", v)
		}
		if v, _ := f["count"].(float64); v != 3 {
			t.Errorf("count = %v, want 3", v)
		}
	}
}

// TestWatchdog_AllSentinelsAbsentAcrossOpTypes runs every tracked SQL op
// end-to-end with a credential-shaped error and asserts none of the
// sentinels reach the file. This is the "never log row content, never
// log interpolated sensitive literals" half, applied to all three ops.
func TestWatchdog_AllSentinelsAbsentAcrossOpTypes(t *testing.T) {
	dm, path := newTestDMWithWatchdog(t)

	ops := []string{watchdogOpExec, watchdogOpQuery, watchdogOpQueryRow}
	for _, op := range ops {
		for _, s := range watchdogSQLSentinels {
			stmt := "INSERT INTO x (v) VALUES ('" + s.value + "')"
			dm.logSQLOutcome(op, stmt, 2*time.Millisecond,
				errors.New("error: "+s.value), 0)
		}
	}

	data, _ := os.ReadFile(path)
	body := string(data)
	for _, s := range watchdogSQLSentinels {
		firstLine := strings.SplitN(s.value, "\n", 2)[0]
		if strings.Contains(body, firstLine) {
			t.Errorf("%s sentinel reached the watchdog log across ops: see %q", s.name, firstLine)
		}
	}
}
