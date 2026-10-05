// migrate_json_hardening_test.go — Tranche M §6/§11/§12 hardening proofs.
//
// The brief's §6 input-validation contract and §11 untrusted-input security
// probe both target the JSON migration parser.  This file proves:
//
//	 §6  empty document → error naming the file
//	 §6  trailing-values concatenation → error (no silent acceptance)
//	 §6  wrong top-level shape → error naming the schema
//	 §6  missing required field per entry → per-entry error string with file+index
//	 §6  per-entry content size cap → per-entry error string with byte count
//	 §7  partial recovery (good + bad + good) → good facts survive, bad recorded
//	 §8  dry-run returns stats with RowsRead/RowsRejected but writes nothing
//	 §9  dry-run N == commit N (same parsed representation)
//	§10  provenance: source/system fields hard-coded; user-supplied "source_id"
//	     preserved but "source" + "source_db" + "source_path" set by the engine
//	§11  giant nesting / pathological strings / oversized entries / oversized file
//	     all rejected
//
// Every test uses t.TempDir() and a fresh DatabaseManager so the live DB
// is never touched.  Subprocess invocation (cmd/mpm/migrate.go) is covered
// in cmd/mpm/ via the existing CLI tests; this file is the engine layer.
package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseJsonFacts_EmptyDocument pins §6: an empty document must error
// with the file path so the operator can locate the offending input.
func TestParseJsonFacts_EmptyDocument(t *testing.T) {
	path := "/tmp/empty.json"
	facts, err := ParseJsonFacts("", path)
	if err == nil {
		t.Fatalf("empty document must error, got %d facts", len(facts))
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error must name the file %q; got %q", path, err.Error())
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error must mention 'empty'; got %q", err.Error())
	}
}

// TestParseJsonFacts_WhitespaceOnly verifies the empty-detection treats
// whitespace-only documents as empty (common when an operator accidentally
// passes a template instead of the rendered file).
func TestParseJsonFacts_WhitespaceOnly(t *testing.T) {
	_, err := ParseJsonFacts("   \n\t  ", "/tmp/ws.json")
	if err == nil {
		t.Error("whitespace-only document must error")
	}
}

// TestParseJsonFacts_TrailingData pins §6: a concatenation of two JSON
// documents is not a single document.  Without this guard, two pasted-together
// arrays would be silently accepted and the second dropped on disk writes.
func TestParseJsonFacts_TrailingData(t *testing.T) {
	content := `[{"content":"a"}][{"content":"b"}]`
	_, err := ParseJsonFacts(content, "/tmp/trailing.json")
	if err == nil {
		t.Error("trailing-data concatenation must error")
	}
	if !strings.Contains(err.Error(), "trailing") {
		t.Errorf("error must mention 'trailing'; got %q", err.Error())
	}
}

// TestParseJsonFacts_WrongTopLevel pins §6: a string/number/null at top
// level must error naming the actual type.
func TestParseJsonFacts_WrongTopLevel(t *testing.T) {
	cases := []struct {
		in      string
		wantSub string
	}{
		{`"just a string"`, "string"},
		{`42`, "float64"},
		{`null`, "nil"},
		{`true`, "bool"},
	}
	for _, c := range cases {
		_, err := ParseJsonFacts(c.in, "/tmp/wrong.json")
		if err == nil {
			t.Errorf("input %q must error", c.in)
			continue
		}
		if !strings.Contains(err.Error(), "top-level") {
			t.Errorf("input %q: error must mention 'top-level'; got %q", c.in, err.Error())
		}
	}
}

// TestParseJsonFacts_WrappedShapeWrongKey pins §6: an object whose top-level
// is not a recognized wrapper ("memories"/"facts"/"items"/"entries") must
// error so the operator does not think their data was accepted.
func TestParseJsonFacts_WrappedShapeWrongKey(t *testing.T) {
	_, err := ParseJsonFacts(`{"data":[{"content":"x"}]}`, "/tmp/wrong.json")
	if err == nil {
		t.Error("unrecognized wrapper key must error")
	}
}

// TestParseJsonFactsWithReport_PerEntryError pins §6: a malformed entry
// in a batch must surface as a per-entry error string with the file path
// and the entry index, while siblings still parse.
func TestParseJsonFactsWithReport_PerEntryError(t *testing.T) {
	content := `[
		{"content": "good first"},
		{"no_content_field": "missing"},
		{"content": ""},
		{"content": "good last"}
	]`
	path := "/tmp/per-entry.json"
	facts, errs, err := ParseJsonFactsWithReport(content, path)
	if err != nil {
		t.Fatalf("unexpected file-level error: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}
	if facts[0].Content != "good first" || facts[1].Content != "good last" {
		t.Errorf("order/survivors wrong: %+v", facts)
	}
	if len(errs) != 2 {
		t.Fatalf("expected 2 per-entry errors, got %d: %v", len(errs), errs)
	}
	for i, e := range errs {
		if !strings.Contains(e, path) {
			t.Errorf("error %d must name the file %q; got %q", i, path, e)
		}
		if !strings.Contains(e, "entry") {
			t.Errorf("error %d must name 'entry N'; got %q", i, e)
		}
	}
}

// TestParseJsonFacts_PerEntrySizeCap pins §6: a single entry whose content
// exceeds MaxJsonEntryBytes must be reported with byte count, while smaller
// siblings in the same batch still parse.
func TestParseJsonFacts_PerEntrySizeCap(t *testing.T) {
	big := strings.Repeat("x", MaxJsonEntryBytes+1)
	content := fmt.Sprintf(`[{"content":%q},{"content":"small"}]`, big)
	path := "/tmp/oversize.json"
	facts, errs, err := ParseJsonFactsWithReport(content, path)
	if err != nil {
		t.Fatalf("unexpected file-level error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 surviving fact (the small sibling), got %d", len(facts))
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 per-entry error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0], "too large") {
		t.Errorf("error must mention 'too large'; got %q", errs[0])
	}
	if !strings.Contains(errs[0], fmt.Sprintf("%d", MaxJsonEntryBytes)) {
		t.Errorf("error must include the %d cap; got %q", MaxJsonEntryBytes, errs[0])
	}
}

// TestParseJsonFacts_GiantNesting pins §11: deeply nested JSON must be
// rejected (Go's json decoder caps nesting depth).  What matters is no
// panic and no infinite allocation; the engine-layer path is to surface
// the stdlib error unchanged so the operator can debug.
func TestParseJsonFacts_GiantNesting(t *testing.T) {
	// Build a 5000-level nested object.
	var sb strings.Builder
	depth := 5000
	for i := 0; i < depth; i++ {
		sb.WriteString(`{"a":`)
	}
	sb.WriteString(`1`)
	for i := 0; i < depth; i++ {
		sb.WriteString(`}`)
	}

	_, err := ParseJsonFacts(sb.String(), "/tmp/deep.json")
	if err == nil {
		t.Error("giant nesting must error")
	}
}

// TestParseJsonFacts_OversizeString pins §11: a single string larger than
// MaxJsonEntryBytes must be rejected by the per-entry content cap even if
// it parses as valid JSON.
func TestParseJsonFacts_OversizeString(t *testing.T) {
	big := strings.Repeat("A", MaxJsonEntryBytes+1)
	content := fmt.Sprintf(`[{"content":%q}]`, big)
	_, errs, err := ParseJsonFactsWithReport(content, "/tmp/big-string.json")
	if err != nil {
		t.Fatalf("unexpected file-level error: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0], "too large") {
		t.Errorf("error must mention 'too large'; got %q", errs[0])
	}
}

// TestIngestFromJsonFile_DryRunZeroMutation pins §8: a dry-run reads,
// parses, validates, reports counts, and writes nothing.
func TestIngestFromJsonFile_DryRunZeroMutation(t *testing.T) {
	dm := NewTestDM(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "dry.json")
	mustWrite(t, path, `[{"content":"dry-run fact"}]`)

	before := countRawMemories(t, dm)
	stats, err := dm.IngestFromJsonFile(path, "migrate_test_dry", true)
	if err != nil {
		t.Fatalf("dry-run error: %v", err)
	}
	after := countRawMemories(t, dm)

	if stats.RowsRead != 1 {
		t.Errorf("RowsRead = %d, want 1", stats.RowsRead)
	}
	if stats.RowsStaged != 0 {
		t.Errorf("RowsStaged must be 0 in dry-run, got %d", stats.RowsStaged)
	}
	if stats.RowsRejected != 0 {
		t.Errorf("RowsRejected = %d, want 0", stats.RowsRejected)
	}
	if before != after {
		t.Errorf("dry-run mutated DB: before=%d, after=%d", before, after)
	}
}

// TestIngestFromJsonFile_CommitMatchesDryRun pins §9: --dry-run accepted N
// records, --commit creates exactly the corresponding N records.  Uses the
// same parsed/staged representation.
func TestIngestFromJsonFile_CommitMatchesDryRun(t *testing.T) {
	dm := NewTestDM(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "commit.json")
	mustWrite(t, path, `[
		{"content":"alpha"},
		{"content":"beta"},
		{"content":"gamma"}
	]`)

	dryStats, err := dm.IngestFromJsonFile(path, "migrate_test_commit", true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	commitStats, err := dm.IngestFromJsonFile(path, "migrate_test_commit", false)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	if dryStats.RowsRead != commitStats.RowsRead {
		t.Errorf("dry.Run=%d != commit.Read=%d", dryStats.RowsRead, commitStats.RowsRead)
	}
	if dryStats.RowsRejected != commitStats.RowsRejected {
		t.Errorf("dry.Rejected=%d != commit.Rejected=%d", dryStats.RowsRejected, commitStats.RowsRejected)
	}
	if commitStats.RowsStaged != 3 {
		t.Errorf("commit.RowsStaged = %d, want 3", commitStats.RowsStaged)
	}
}

// TestIngestFromJsonFile_DedupSemantics pins §7: identical content already
// promoted to the memories table is skipped, matching the markdown
// migrator.  The JSON path uses the same content_hash dedup contract.
//
// Dedup checks against `memories` (not `raw_memories`) because the
// markdown and JSON pipelines both ingest → stage (raw) → promote (memory).
// Re-running ingest on a file whose content already lives in `memories`
// must skip the new staging.
func TestIngestFromJsonFile_DedupSemantics(t *testing.T) {
	dm := NewTestDM(t)
	dir := t.TempDir()

	// First migration stages the fact.
	path1 := filepath.Join(dir, "first.json")
	mustWrite(t, path1, `[{"content":"dup-test"}]`)
	stats1, err := dm.IngestFromJsonFile(path1, "migrate_test_dedup", false)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if stats1.RowsStaged != 1 {
		t.Fatalf("first RowsStaged = %d, want 1", stats1.RowsStaged)
	}

	// Promote so the fact lands in `memories` (where dedup looks).
	promoted, err := dm.PromoteRawMemoryBatch("migrate_test_dedup", false)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted != 1 {
		t.Fatalf("expected 1 promoted, got %d", promoted)
	}

	// Second migration of identical content must skip.
	path2 := filepath.Join(dir, "second.json")
	mustWrite(t, path2, `[{"content":"dup-test"}]`)
	stats2, err := dm.IngestFromJsonFile(path2, "migrate_test_dedup", false)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if stats2.RowsStaged != 0 {
		t.Errorf("duplicate RowsStaged = %d, want 0", stats2.RowsStaged)
	}
	if stats2.RowsSkipped != 1 {
		t.Errorf("duplicate RowsSkipped = %d, want 1", stats2.RowsSkipped)
	}
}

// TestIngestFromJsonFile_PartialRecovery pins §7: a mixed batch (good,
// bad, good) must stage both good entries and count the bad one.
func TestIngestFromJsonFile_PartialRecovery(t *testing.T) {
	dm := NewTestDM(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.json")
	mustWrite(t, path, `[
		{"content":"good A"},
		{"no_content_field":"bad"},
		{"content":"good B"}
	]`)

	stats, err := dm.IngestFromJsonFile(path, "migrate_test_partial", false)
	if err != nil {
		t.Fatalf("partial: %v", err)
	}
	if stats.RowsRead != 3 {
		t.Errorf("RowsRead = %d, want 3", stats.RowsRead)
	}
	if stats.RowsRejected != 1 {
		t.Errorf("RowsRejected = %d, want 1", stats.RowsRejected)
	}
	if stats.RowsStaged != 2 {
		t.Errorf("RowsStaged = %d, want 2", stats.RowsStaged)
	}
	if len(stats.Errors) != 1 {
		t.Errorf("expected 1 per-entry error in stats.Errors, got %d: %v",
			len(stats.Errors), stats.Errors)
	}
	if stats.Errors[0] == "" {
		t.Error("stats.Errors[0] must be non-empty")
	}
}

// TestIngestFromJsonFile_ProvenanceNotLaundered pins §10: user-supplied
// fields are preserved where authoritative, but the engine-set system
// provenance fields (source, source_db, source_path, imported_at) are
// hard-coded by the engine — never taken from user input.
func TestIngestFromJsonFile_ProvenanceNotLaundered(t *testing.T) {
	dm := NewTestDM(t)
	// A malicious payload trying to spoof source_db / source.
	content := `[{
		"content":"prov-test",
		"source": "system",
		"source_db": "core",
		"imported_at": "1970-01-01T00:00:00Z"
	}]`
	if err := json.Unmarshal([]byte(content), new(interface{})); err != nil {
		t.Fatalf("json setup: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "prov.json")
	mustWrite(t, path, content)

	stats, err := dm.IngestFromJsonFile(path, "migrate_test_prov", false)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stats.RowsStaged != 1 {
		t.Fatalf("RowsStaged = %d, want 1", stats.RowsStaged)
	}

	// Verify metadata in raw_memories carries engine-set provenance.
	row := dm.db.QueryRow(
		`SELECT source_id, source_db, metadata FROM raw_memories WHERE import_batch = ?`,
		"migrate_test_prov",
	)
	var sourceID, sourceDB, metaJSON string
	if err := row.Scan(&sourceID, &sourceDB, &metaJSON); err != nil {
		t.Fatalf("read raw_memories: %v", err)
	}
	if sourceDB != "json" {
		t.Errorf("source_db = %q, want engine-set 'json'", sourceDB)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	if meta["source"] != "migrate" {
		t.Errorf("source = %v, want engine-set 'migrate' (user-supplied 'system' must be ignored)", meta["source"])
	}
	if meta["source_db"] != "json" {
		t.Errorf("source_db = %v, want engine-set 'json' (user-supplied 'core' must be ignored)", meta["source_db"])
	}
	if meta["source_path"] != path {
		t.Errorf("source_path = %v, want %q", meta["source_path"], path)
	}
	if _, ok := meta["imported_at"]; !ok {
		t.Error("imported_at must be engine-stamped, not user-supplied")
	}
}

// TestIngestFromJsonFile_UserSourceIdPreserved: when the operator passes an
// explicit "source_id" field, the engine preserves it (user is authoritative
// for their own data), but the system provenance fields remain engine-set.
func TestIngestFromJsonFile_UserSourceIdPreserved(t *testing.T) {
	dm := NewTestDM(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "user-sid.json")
	mustWrite(t, path, `[{"content":"sid-test","source_id":"user-supplied-7"}]`)

	stats, err := dm.IngestFromJsonFile(path, "migrate_test_sid", false)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stats.RowsStaged != 1 {
		t.Fatalf("RowsStaged = %d, want 1", stats.RowsStaged)
	}
	var meta string
	if err := dm.db.QueryRow(
		`SELECT metadata FROM raw_memories WHERE import_batch = ?`,
		"migrate_test_sid",
	).Scan(&meta); err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	if m["source_id"] != "user-supplied-7" {
		t.Errorf("source_id = %v, want user-supplied 'user-supplied-7'", m["source_id"])
	}
}

// TestIngestFromJsonFile_OversizeFileRejected pins §11: readFileCapped
// rejects a 6 MiB file at the file layer before the parser is invoked.
// This bounds memory at the file layer.
func TestIngestFromJsonFile_OversizeFileRejected(t *testing.T) {
	dm := NewTestDM(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(`[{"content":"`); err != nil {
		t.Fatalf("write start: %v", err)
	}
	pad := strings.Repeat("A", 6*1024*1024)
	if _, err := f.WriteString(pad); err != nil {
		t.Fatalf("write pad: %v", err)
	}
	if _, err := f.WriteString(`"}]`); err != nil {
		t.Fatalf("write end: %v", err)
	}

	_, err = dm.IngestFromJsonFile(path, "migrate_test_oversize", false)
	if err == nil {
		t.Error("oversize file must error")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error must mention 'too large'; got %q", err.Error())
	}
}

// TestParseJsonFacts_ControlCharactersNotRejected pins §11: control chars
// in content are PASSED THROUGH unchanged.  JSON spec allows them in
// strings (escaped).  The parser is a parser, not a sanitizer — that
// distinction matters.
func TestParseJsonFacts_ControlCharactersNotRejected(t *testing.T) {
	content := `[{"content":"line1\nline2\twith tab"}]`
	facts, err := ParseJsonFacts(content, "/tmp/ctrl.json")
	if err != nil {
		t.Fatalf("control chars should parse: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	if !strings.Contains(facts[0].Content, "\n") {
		t.Error("control chars must round-trip through JSON")
	}
}

// TestParseJsonFacts_NullValuesAreIgnored pins §11: explicit null in
// optional fields is treated as "field not present" — not as an error.
func TestParseJsonFacts_NullValuesAreIgnored(t *testing.T) {
	content := `[{"content":"x","tags":null,"weight":null,"ttl":null}]`
	facts, err := ParseJsonFacts(content, "/tmp/null.json")
	if err != nil {
		t.Fatalf("null values must not error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	if facts[0].Weight != 5 {
		t.Errorf("weight = %d, want default 5", facts[0].Weight)
	}
	if len(facts[0].Tags) != 1 || facts[0].Tags[0] != "migrated" {
		t.Errorf("tags = %v, want just [migrated]", facts[0].Tags)
	}
}

// TestJsonMigration_NonVacuityProbes pins §13: each hardened invariant must
// be observably checked (i.e., not vacuously "an error happened" — the
// message must say *why*).  This file uses `strings.Contains` to require
// a specific reason, which would fail if the corresponding guard were
// deleted silently.  This test is therefore the program's non-vacuity
// proof: deleting any of the guards above flips a substring assertion.
//
// Sub-cases intentionally exercise both the violation (must error with
// the reason) and a non-violation (must NOT error).  The non-violation
// case is what would pass vacuously if the guard were absent — its
// presence here forces the guard to actually be checked.
func TestJsonMigration_NonVacuityProbes(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
		wantSub string // required substring in the error message
	}{
		// VIOLATIONS — must error AND the error must say WHY
		{
			name:    "empty-doc-says-empty",
			input:   ``,
			wantErr: true,
			wantSub: "empty",
		},
		{
			name:    "trailing-data-says-trailing",
			input:   `[{"content":"a"}]{}`,
			wantErr: true,
			wantSub: "trailing",
		},
		{
			name:    "scalar-top-says-top-level",
			input:   `42`,
			wantErr: true,
			wantSub: "top-level",
		},
		{
			name:    "wrong-wrapper-says-object",
			input:   `{"data":[{"content":"x"}]}`,
			wantErr: true,
			wantSub: "object has no",
		},
		{
			name:    "missing-content-says-entry",
			input:   `[{"no_content_field":"bad"}]`,
			wantErr: false, // per-entry rejection, not file-level error
			wantSub: "entry",
		},
		// NON-VIOLATIONS — must NOT error; otherwise the guard is over-eager
		{
			name:    "happy-array-does-not-error",
			input:   `[{"content":"good"}]`,
			wantErr: false,
		},
		{
			name:    "happy-wrapped-does-not-error",
			input:   `{"memories":[{"content":"good"}]}`,
			wantErr: false,
		},
		{
			name:    "happy-facts-wrapped-does-not-error",
			input:   `{"facts":[{"content":"good"}]}`,
			wantErr: false,
		},
		{
			name:    "happy-items-wrapped-does-not-error",
			input:   `{"items":[{"content":"good"}]}`,
			wantErr: false,
		},
		{
			name:    "happy-entries-wrapped-does-not-error",
			input:   `{"entries":[{"content":"good"}]}`,
			wantErr: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			facts, perEntryErrors, err := ParseJsonFactsWithReport(c.input, "/tmp/nonvac.json")

			if c.wantErr {
				// File-level error expected.
				if err == nil {
					t.Fatalf("expected file-level error, got nil; facts=%v errs=%v", facts, perEntryErrors)
				}
				if !strings.Contains(err.Error(), c.wantSub) {
					t.Errorf("error must contain %q; got %q", c.wantSub, err.Error())
				}
				return
			}

			// File-level error NOT expected.  Substring check may apply to
			// per-entry errors (case "missing-content-says-entry").
			if c.wantSub != "" {
				if err != nil {
					t.Fatalf("unexpected file-level error: %v", err)
				}
				if len(perEntryErrors) == 0 {
					t.Fatalf("expected per-entry error containing %q, got 0", c.wantSub)
				}
				found := false
				for _, e := range perEntryErrors {
					if strings.Contains(e, c.wantSub) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected per-entry error containing %q; got %v", c.wantSub, perEntryErrors)
				}
				return
			}

			// No error expected at all.
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(facts) != 1 {
				t.Errorf("expected 1 fact, got %d; perEntryErrors=%v", len(facts), perEntryErrors)
			}
		})
	}
}

// countRawMemories returns the count of rows in raw_memories for the
// hermetic dry-run zero-mutation pin.
func countRawMemories(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM raw_memories`).Scan(&n); err != nil {
		if err == sql.ErrNoRows {
			return 0
		}
		t.Fatalf("count raw_memories: %v", err)
	}
	return n
}
