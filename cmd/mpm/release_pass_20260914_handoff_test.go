// release_pass_20260914_handoff_test.go — Regression coverage for
// the 2026-09-14 release-pass handoff wire-contract defects.
//
// Two specific defects closed in this pass:
//
//   Bug A — response-key mismatch. The substrate emitted
//   {success, count, results}; the CLI read `out["handoffs"]`,
//   a key the substrate never wrote. The CLI therefore always
//   saw an empty slice and rendered "No handoffs" even when
//   real rows existed.
//
//   Bug B — unread parameter mismatch. The CLI sent
//   `unread_only`; the substrate read `unread`. The unread
//   filter was silently ignored.
//
// Both bugs were present simultaneously so the visible symptom
// was "handoff list always empty regardless of state". Fixing
// one without the other would have looked like a partial
// recovery. The regressions pin BOTH.
//
// All tests use a hermetic workspace via t.TempDir(); production
// state is never touched.

package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// handoffTestBin builds a fresh mpm binary in a temp dir so the
// tests run hermetically without depending on the operator's
// pre-built ./bin/mpm. The build is cached per (test process,
// package) — Go's build cache handles the rest.
func handoffTestBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-test")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// newBuffer is a tiny bytes.Buffer constructor; centralised so
// the helpers don't repeat the allocation. Returns a fresh
// buffer per call.
func newBuffer() *bytes.Buffer { return &bytes.Buffer{} }

// writeHandoffs seeds N handoffs via the substrate write action
// so the tests don't depend on the CLI write path (which has a
// separate "id=" display bug not in scope for this pass).
// Returns the handoff ids in the order they were written.
func writeHandoffs(t *testing.T, bin, ws string, summaries []string) []string {
	t.Helper()
	ids := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		payload := `{"action":"write","params":{"summary":"` + summary + `"}}`
		cmd := exec.Command(bin, "call", "mpm_handoff", "--payload", payload)
		cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("write handoff: %v\n%s", err, out)
		}
		// Substrate emits {"handoff_id": "<id>", ...}; pull the id
		// out without depending on the CLI's broken display.
		var env struct {
			HandoffID string `json:"handoff_id"`
		}
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatalf("parse handoff write envelope: %v\n%s", err, out)
		}
		if env.HandoffID == "" {
			t.Fatalf("write handoff returned empty id; raw: %s", out)
		}
		ids = append(ids, env.HandoffID)
	}
	return ids
}

// markHandoffRead marks a specific handoff as read via the
// substrate's read action with mark_read=true.
func markHandoffRead(t *testing.T, bin, ws, id string) {
	t.Helper()
	payload := `{"action":"read","params":{"handoff_id":"` + id + `","mark_read":true}}`
	cmd := exec.Command(bin, "call", "mpm_handoff", "--payload", payload)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mark handoff read: %v\n%s", err, out)
	}
}

// lookupTestPath returns a minimal PATH so exec.Command can find
// the binary when invoked from a hermetic test environment.
func lookupTestPath() string {
	return "/usr/bin:/bin:/usr/local/go/bin"
}

// TestHandoff_ListReturnsAllRows is the headline regression:
// with multiple rows present, `mpm handoff list` returns them
// (not zero). Pre-fix this assertion failed because the CLI
// read the wrong key and always saw an empty slice.
func TestHandoff_ListReturnsAllRows(t *testing.T) {
	bin := handoffTestBin(t)
	ws := t.TempDir()
	ids := writeHandoffs(t, bin, ws, []string{"alpha probe", "beta probe", "gamma probe"})

	out := runHandoffList(t, bin, ws)
	if !strings.Contains(out, "3 handoffs") {
		t.Fatalf("default handoff list must report 3 handoffs; got:\n%s", out)
	}
	for _, want := range []string{
		"alpha probe", "beta probe", "gamma probe",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("handoff list missing %q in:\n%s", want, out)
		}
	}
	_ = ids // ids used implicitly via DB state; not asserted here
}

// TestHandoff_JSONEnvelopeMatchesHumanCount asserts human and
// JSON modes return the same number of handoffs. Pre-fix JSON
// always emitted count=0 regardless of DB state.
func TestHandoff_JSONEnvelopeMatchesHumanCount(t *testing.T) {
	bin := handoffTestBin(t)
	ws := t.TempDir()
	writeHandoffs(t, bin, ws, []string{"one probe", "two probe"})

	humanOut := runHandoffList(t, bin, ws)
	jsonOut := runHandoffList(t, bin, ws, "--json")

	env := parseHandoffListEnvelope(t, jsonOut)
	if env.Count != 2 {
		t.Fatalf("JSON envelope count = %d, want 2; envelope: %+v", env.Count, env)
	}
	if len(env.Results) != env.Count {
		t.Fatalf("JSON envelope results length (%d) != count (%d)", len(env.Results), env.Count)
	}
	if !strings.Contains(humanOut, "2 handoffs") {
		t.Fatalf("human list must report 2 handoffs; got:\n%s", humanOut)
	}
}

// TestHandoff_UnreadFiltersReadRows asserts --unread excludes
// handoffs that have already been marked as read. Pre-fix the
// CLI sent `unread_only` and the substrate read `unread`, so
// the filter was silently ignored and --unread returned the
// same as the default list.
func TestHandoff_UnreadFiltersReadRows(t *testing.T) {
	bin := handoffTestBin(t)
	ws := t.TempDir()
	ids := writeHandoffs(t, bin, ws, []string{"first", "second", "third"})

	// Mark the middle one as read. After this:
	//   first  — unread
	//   second — read
	//   third  — unread
	markHandoffRead(t, bin, ws, ids[1])

	unreadOut := runHandoffList(t, bin, ws, "--unread")
	if !strings.Contains(unreadOut, "2 handoffs") {
		t.Fatalf("--unread must report 2 handoffs (read row excluded); got:\n%s", unreadOut)
	}
	if strings.Contains(unreadOut, "second") {
		t.Fatalf("--unread must exclude the read handoff `second`; got:\n%s", unreadOut)
	}
	for _, want := range []string{"first", "third"} {
		if !strings.Contains(unreadOut, want) {
			t.Errorf("--unread missing %q in:\n%s", want, unreadOut)
		}
	}
}

// TestHandoff_HumanAndJSONShareSameRows asserts both modes
// reference the same underlying data. Pre-fix JSON always
// returned 0; this assertion pins equality of the row set
// across both modes for the same DB state.
func TestHandoff_HumanAndJSONShareSameRows(t *testing.T) {
	bin := handoffTestBin(t)
	ws := t.TempDir()
	writeHandoffs(t, bin, ws, []string{"shared row one", "shared row two"})

	humanOut := runHandoffList(t, bin, ws)
	jsonOut := runHandoffList(t, bin, ws, "--json")
	env := parseHandoffListEnvelope(t, jsonOut)

	if env.Count != 2 {
		t.Fatalf("JSON envelope count = %d, want 2", env.Count)
	}
	// Every id present in JSON must appear in human output too.
	for _, row := range env.Results {
		id, _ := row["id"].(string)
		if id == "" {
			continue
		}
		if !strings.Contains(humanOut, id) {
			t.Errorf("human output missing id %q from JSON envelope; human:\n%s", id, humanOut)
		}
	}
}

// handoffListEnvelope is the canonical wire shape for
// `mpm handoff list --json`. The substrate emits `results`
// after the 2026-09-14 release-pass wire-contract fix; the
// CLI emits the same key. Pin both here.
type handoffListEnvelope struct {
	Success bool                     `json:"success"`
	Count   int                      `json:"count"`
	Results []map[string]interface{} `json:"results"`
}

func parseHandoffListEnvelope(t *testing.T, raw string) handoffListEnvelope {
	t.Helper()
	idx := strings.IndexByte(raw, '{')
	if idx < 0 {
		t.Fatalf("no JSON envelope in output:\n%s", raw)
	}
	var env handoffListEnvelope
	if err := json.Unmarshal([]byte(raw[idx:]), &env); err != nil {
		t.Fatalf("parse handoff list envelope: %v\nraw:\n%s", err, raw[idx:])
	}
	return env
}

// runHandoffList invokes `mpm handoff list [--unread] [--json]`
// in the hermetic workspace and returns stdout.
func runHandoffList(t *testing.T, bin, ws string, opts ...string) string {
	t.Helper()
	args := []string{"handoff", "list"}
	args = append(args, opts...)
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("mpm %s: %v\nstderr: %s", strings.Join(args, " "), err, newBuffer())
	}
	return string(out)
}
