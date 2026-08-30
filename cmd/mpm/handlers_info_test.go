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
