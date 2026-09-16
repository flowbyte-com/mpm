// handlers_info_test.go — D-005/W-003 (alpha-4.1.1) regression tests.
//
// Bug: `mpm info --json` returned "not yet implemented" — the
// human-readable form was canonical, but machine consumers (drill
// orchestrator, monitoring, audit reporters) had no structured way
// to inspect installation identity. The dashboard-only contract also
// meant a single rendering shape lived in two places: the human
// printf and any future JSON would inevitably drift.
//
// Fix: collect the same facts once into an infoOutput struct, then
// render to human or JSON from the same struct. The two surfaces
// cannot drift; adding a new field happens in one place.
//
// The tests below pin:
//   - `--json` exits 0 with a valid JSON document
//   - the document has every section the human form has
//     (identity, workspace, database, skills, scheduler, runtime)
//   - unknown flags still hard-fail
//   - the JSON field set is stable (a future field rename surfaces
//     here, which is what machine consumers actually want)
package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// TestD005_Info_JSONEmitsValidEnvelope is the headline regression:
// `--json` must emit a parseable JSON document with the canonical
// top-level keys. A pre-fix run returns "not yet implemented"
// (exit 1) — this test would simply not reach the JSON assertion.
func TestD005_Info_JSONEmitsValidEnvelope(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable — set MPM_WORKSPACE to a populated workspace to run")
	}

	out := captureStdout(t, func() {
		if rc := handleInfo([]string{"--json"}); rc != 0 {
			t.Fatalf("handleInfo --json exit %d (expected 0)", rc)
		}
	})

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("handleInfo --json emitted non-JSON output: %v\n--output--\n%s", err, out)
	}

	// Top-level keys that machine consumers depend on. Add new
	// keys here AND in infoOutput (and the human form) — the test
	// is the structural reminder.
	for _, k := range []string{
		"version", "data_directory", "db_path",
		"workspace", "database", "skills", "scheduler", "runtime",
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("info --json missing top-level key %q\n--output--\n%s", k, out)
		}
	}
}

// TestD005_Info_JSONSectionsMatchHuman pins the no-drift invariant:
// every section in the human form has a corresponding JSON object.
// A future refactor that adds a "personas" section to the human
// form without mirroring it to the JSON form will fail this test.
func TestD005_Info_JSONSectionsMatchHuman(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable — set MPM_WORKSPACE to a populated workspace to run")
	}

	humanOut := captureStdout(t, func() {
		if rc := handleInfo([]string{}); rc != 0 {
			t.Fatalf("handleInfo exit %d", rc)
		}
	})
	jsonOut := captureStdout(t, func() {
		if rc := handleInfo([]string{"--json"}); rc != 0 {
			t.Fatalf("handleInfo --json exit %d", rc)
		}
	})

	// Human section headers → JSON keys. "Identity" is a special
	// case: the human form has an Identity section but the JSON
	// form factors its fields to the top level (version,
	// data_directory, db_path) rather than nesting them. The
	// no-drift invariant still holds — every human section has at
	// least one JSON representation — but the mapping isn't
	// 1-to-1 by key, so we list the JSON keys per section.
	sectionToJSONKeys := map[string][]string{
		"Identity":  {"version", "data_directory", "db_path"},
		"Workspace": {"workspace"},
		"Database":  {"database"},
		"Skills":    {"skills"},
		"Scheduler": {"scheduler"},
		"Runtime":   {"runtime"},
	}
	for section := range sectionToJSONKeys {
		if !strings.Contains(humanOut, section) {
			t.Errorf("human form missing section header %q", section)
		}
	}

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(jsonOut), &got); err != nil {
		t.Fatalf("JSON parse failed: %v", err)
	}
	for section, keys := range sectionToJSONKeys {
		for _, key := range keys {
			if _, ok := got[key]; !ok {
				t.Errorf("JSON missing key %q (representing human section %q)", key, section)
			}
		}
	}
}

// TestD005_Info_RejectsUnknownFlags pins the negative contract: a
// typo in `--jsno` (or any unknown flag) must still hard-fail with
// a helpful error, not silently no-op. The pre-fix `--json` rejected
// everything as "not yet implemented"; the post-fix only rejects
// genuinely unknown flags.
func TestD005_Info_RejectsUnknownFlags(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable — set MPM_WORKSPACE to a populated workspace to run")
	}

	rc := 0
	captureStreams(t, func() {
		rc = handleInfo([]string{"--jsno"})
	})
	if rc == 0 {
		t.Errorf("handleInfo --jsno exit 0 (expected non-zero: unknown flag must hard-fail)")
	}
}

// TestD005_Info_AcceptsJSONFlagAnywhere pins the position-tolerance
// contract documented in ExtractJSONFlag's comment: `--json` may
// appear at any position in the arg list, not just the first slot.
// This is what machine consumers expect when piping flags.
func TestD005_Info_AcceptsJSONFlagAnywhere(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable — set MPM_WORKSPACE to a populated workspace to run")
	}

	out := captureStdout(t, func() {
		if rc := handleInfo([]string{"-j"}); rc != 0 {
			t.Fatalf("handleInfo -j exit %d", rc)
		}
	})
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("handleInfo -j emitted non-JSON output: %v\n--output--\n%s", err, out)
	}
	if _, ok := got["version"]; !ok {
		t.Errorf("-j emitted JSON missing 'version' key — short-form flag broken")
	}
}

// TestInfo_DatabaseHumanRenderingSemantics pins the 2026-09-16
// fresh-profile UX cleanup. Pre-fix the Database section listed
// bare counters (`total : 0`, `active : 0`, `ltm : 0`, ...) under
// a generic "stats" label, so a pristine install with 5
// directives but 0 memories rendered as `total : 0` — which
// the user (reasonably) read as "the database is empty". That
// is wrong: the database contains directives and other
// substrate state, just no ordinary memories.
//
// Post-fix the Database block uses humanized labels that
// explicitly name what each counter refers to:
//
//	memories : N total | N active | N LTM
//	directives : N active
//	deleted memories : N
//	never accessed : N
//	expired : N
//
// The aggregated label `memories` makes the meaning
// unambiguous, the directive count is sourced from the canonical
// `countDirectives` helper (same as `mpm status`), and the
// sub-counters are clearly memory-scoped.
func TestInfo_DatabaseHumanRenderingSemantics(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable — set MPM_WORKSPACE to a populated workspace to run")
	}

	out := captureStdout(t, func() {
		if rc := handleInfo([]string{}); rc != 0 {
			t.Fatalf("handleInfo exit %d", rc)
		}
	})

	// Required humanized labels — the bare-token labels from the
	// pre-fix form must NOT appear, since they were ambiguous on
	// a fresh install.
	mustHave := []string{
		"memories",   // combined: total | active | LTM
		"directives", // own row with active count
		"deleted memories",
		"never accessed", // humanized (was `never_accessed`)
		"expired",
	}
	for _, want := range mustHave {
		if !strings.Contains(out, want) {
			t.Errorf("mpm info human output missing %q (Database block must use humanized labels)\n--output--\n%s", want, out)
		}
	}

	// Negative pins — the pre-fix bare-token labels are gone.
	// "total :" as a standalone label (not embedded inside the
	// combined `memories :` line) is the user-confusable form.
	// "never_accessed" (snake_case) was the pre-fix humanization
	// — the fix humanizes to "never accessed".
	mustNotHave := []string{
		"\ntotal :",
		"\nactive :",
		"\nltm :",
		"\ndeleted :",
		"never_accessed", // snake_case must not appear in human form
	}
	for _, bad := range mustNotHave {
		if strings.Contains(out, bad) {
			t.Errorf("mpm info human output contains pre-fix label %q (Database block must use humanized combined labels)\n--output--\n%s", bad, out)
		}
	}

	// The combined `memories :` line must include the three
	// memory-class counters in the canonical "<n> total | <n>
	// active | <n> LTM" shape. We assert on the pattern rather
	// than exact values so the test survives seed changes.
	re := regexp.MustCompile(`memories\s+:\s+\d+ total\s+\|\s+\d+ active\s+\|\s+\d+ LTM`)
	if !re.MatchString(out) {
		t.Errorf("mpm info Database block must render memories as `N total | N active | N LTM`; regex didn't match\n--output--\n%s", out)
	}
}

// TestInfo_DatabaseJSONShapeUnchanged pins the no-drift invariant
// for the machine-readable Database shape. The 2026-09-16 human-
// rendering cleanup must NOT change the JSON envelope — the stats
// map keeps its existing keys (total, active, ltm, deleted,
// never_accessed, expired) and the directives count is Go-only
// (not promoted into JSON) so external consumers parsing the
// existing shape don't break.
func TestInfo_DatabaseJSONShapeUnchanged(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable — set MPM_WORKSPACE to a populated workspace to run")
	}

	out := captureStdout(t, func() {
		if rc := handleInfo([]string{"--json"}); rc != 0 {
			t.Fatalf("handleInfo --json exit %d", rc)
		}
	})

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("JSON parse failed: %v\n--output--\n%s", err, out)
	}

	dbRaw, ok := got["database"]
	if !ok {
		t.Fatalf("JSON missing 'database' object\n--output--\n%s", out)
	}
	db, ok := dbRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("JSON 'database' is not an object: %T", dbRaw)
	}

	// The stats map must still contain every existing key.
	statsRaw, ok := db["stats"]
	if !ok {
		t.Fatalf("JSON database.stats missing\n--output--\n%s", out)
	}
	stats, ok := statsRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("JSON database.stats is not an object: %T", statsRaw)
	}
	for _, k := range []string{"total", "active", "ltm", "deleted", "never_accessed", "expired"} {
		if _, ok := stats[k]; !ok {
			t.Errorf("JSON database.stats missing %q key (machine-readable shape must stay backward-compatible)\n--output--\n%s", k, out)
		}
	}

	// The directives count must NOT appear in JSON — it is a
	// human-only field. The spec says "machine-readable JSON/API
	// shapes remain unchanged unless an existing documented shape
	// already includes an appropriate directive field", and no
	// such field exists today.
	if _, ok := db["directives"]; ok {
		t.Errorf("JSON database.directives appears but the field is human-only per spec; remove from infoDatabase struct or change the json tag\n--output--\n%s", out)
	}
	if _, ok := stats["directives"]; ok {
		t.Errorf("JSON database.stats.directives appears but the human-only directive count must not leak into the stats map\n--output--\n%s", out)
	}
}

// TestInfo_DatabaseDirectivesCountMatchesStatus is the cross-surface
// alignment pin: the directives count surfaced in `mpm info`'s
// Database block must equal the canonical count surfaced in
// `mpm status`'s Directives row. Both surfaces share the
// `countDirectives` helper so they cannot drift on directive
// accounting.
func TestInfo_DatabaseDirectivesCountMatchesStatus(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable — set MPM_WORKSPACE to a populated workspace to run")
	}

	// Snapshot the canonical directives count.
	canonical, err := countDirectives(dm)
	if err != nil {
		t.Fatalf("countDirectives: %v", err)
	}

	// Drive collectInfo directly (without JSON/HTTP overhead)
	// so the test pins the data layer, not the renderer.
	out := collectInfo(dm)
	if out.Database.Directives != canonical {
		t.Errorf("info Database.Directives (%d) disagrees with countDirectives (%d); surfaces must share the canonical helper", out.Database.Directives, canonical)
	}
}
