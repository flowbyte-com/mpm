package main

// Alpha-4.1 F-005 + W-006 regression: `mpm memory search --json` must
// not join "--json" into the search query.
//
// Pre-fix bug:
//
//	mpm memory search "Q3"        → 1 hit
//	mpm memory search "Q3" --json → 0 hits (because the literal string
//	                                "--json" was appended to "Q3" before
//	                                the FTS5 query ran)
//
// The contract is:
//   - `--json` / `-j` is stripped from the argument list before the
//     query is constructed.
//   - When `--json` is passed, the human-readable formatter is
//     bypassed and a JSON envelope is emitted instead.
//   - Short option `-j` is supported.
//   - When no flag is passed, the existing human-readable behavior is
//     preserved.
//   - The same fix applies symmetrically to memory add / list / show
//     where the audit's W-006 review found the same shape.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestF005_SearchJSONFlag_DoesNotPolluteQuery is the public-boundary
// test: passing --json alongside a query must NOT change the FTS5
// recall behavior. Pre-fix this returned zero hits because the
// literal "--json" token was joined into the query string.
func TestF005_SearchJSONFlag_DoesNotPolluteQuery(t *testing.T) {
	// Inject a memorable token and ensure the search sees it
	// regardless of the --json flag. The token is a single word
	// (no hyphens) so it exercises the FTS5 / LIKE fallback path
	// cleanly — the F-005 regression is about the flag, not about
	// hyphenated FTS5 syntax, which is exercised by QueryMemory's
	// quote-wrapping path on its own.
	store := getMemoryStore()
	mkMemoryForTest(t, store, "f005-marker", "alpha-4.1 f005 marker token content", "f005-search")

	hitsHuman := runMemorySearch(t, "marker")
	if !strings.Contains(hitsHuman, "f005-marker") {
		t.Fatalf("human search missing the f005 hit; got: %q", hitsHuman)
	}
	hitsJSON := runMemorySearch(t, "marker", "--json")
	if !strings.Contains(hitsJSON, "f005-marker") {
		t.Fatalf("--json search missing the f005 hit (flag was joined into query); got: %q", hitsJSON)
	}
	hitsShort := runMemorySearch(t, "marker", "-j")
	if !strings.Contains(hitsShort, "f005-marker") {
		t.Fatalf("-j search missing the f005 hit (short flag was joined into query); got: %q", hitsShort)
	}
}

// TestF005_SearchJSONFlag_EmitsJSONEnvelope confirms that --json / -j
// flips the output to a JSON envelope, not a human-readable block.
func TestF005_SearchJSONFlag_EmitsJSONEnvelope(t *testing.T) {
	store := getMemoryStore()
	mkMemoryForTest(t, store, "f005-jsonfmt", "alpha-4.1 jsonfmt envelope check", "f005-json")

	out := runMemorySearch(t, "f005-jsonfmt", "--json")
	var env map[string]interface{}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("--json output must be valid JSON; got %q (err=%v)", out, err)
	}
	if _, has := env["memories"]; !has {
		t.Errorf("--json envelope missing 'memories' field: %v", env)
	}
}

// TestF005_SearchJSONFlag_PreservesHumanDefault confirms that without
// --json the existing human-readable text output is preserved.
func TestF005_SearchJSONFlag_PreservesHumanDefault(t *testing.T) {
	store := getMemoryStore()
	mkMemoryForTest(t, store, "f005-human", "alpha-4.1 human default humanword check", "f005-human")

	out := runMemorySearch(t, "f005-human")
	if !strings.Contains(out, "Found") {
		t.Errorf("default search must emit human 'Found N memories' header; got: %q", out)
	}
}

// TestF005_SearchJSONFlag_FlagInAnyPosition pins that --json / -j is
// stripped regardless of where in the argument list it appears.
func TestF005_SearchJSONFlag_FlagInAnyPosition(t *testing.T) {
	store := getMemoryStore()
	mkMemoryForTest(t, store, "f005-mid", "alpha-4.1 mid flag midword check", "f005-mid")

	for _, args := range [][]string{
		{"--json", "f005-mid"},
		{"f005-mid", "--json"},
		{"-j", "f005-mid"},
		{"f005-mid", "-j"},
	} {
		out := runMemorySearch(t, args...)
		if !strings.Contains(out, "f005-mid") {
			t.Errorf("flag-in-position %v dropped the hit; got: %q", args, out)
		}
	}
}

// runMemorySearch runs the handler directly (same path the CLI uses
// when no subprocess is involved). The handler reads from the global
// memory store; mkMemoryForTest seeds the row first.
func runMemorySearch(t *testing.T, args ...string) string {
	t.Helper()
	// Build a synthetic argv so the handler reads the same shape the
	// CLI parses: `mpm memory search <args>`.
	argv := append([]string{"mpm", "memory", "search"}, args...)
	old := saveArgv(argv)
	defer restoreArgv(old)
	// Capture stdout/stderr via the respond() shim — we use a
	// custom one for tests.
	return captureRespond(t, func() int { return handleMemorySearch(args) })
}

// TestF005_SearchJSONFlag_Subprocess is the public-boundary test
// against the actual `mpm` binary: `mpm memory search "<query>" --json`
// must return the same hits as `mpm memory search "<query>"` and
// emit parseable JSON.
func TestF005_SearchJSONFlag_Subprocess(t *testing.T) {
	if mpmBin == "" {
		t.Skip("mpm binary not built; skipping subprocess regression")
	}
	workspace := t.TempDir()
	mkMemoryForTestInWorkspace(t, workspace, "f005-subproc", "alpha-4.1 subprocess subprocword json check", "f005-subproc")

	// First: human-readable baseline.
	stdoutHuman, _, err := callMPMForMemSearch(t, workspace, "memory", "search", "subprocword")
	if err != nil {
		t.Fatalf("human call failed: %v", err)
	}
	if !strings.Contains(stdoutHuman, "f005-subproc") {
		t.Fatalf("human baseline missing f005-subproc; got: %q", stdoutHuman)
	}

	// Then: with --json.
	stdoutJSON, _, err := callMPMForMemSearch(t, workspace, "memory", "search", "subprocword", "--json")
	if err != nil {
		t.Fatalf("json call failed: %v", err)
	}
	var env map[string]interface{}
	if jerr := json.Unmarshal([]byte(stdoutJSON), &env); jerr != nil {
		t.Fatalf("--json output must be valid JSON; got %q (err=%v)", stdoutJSON, jerr)
	}
	if _, has := env["memories"]; !has {
		t.Errorf("--json envelope missing 'memories' field: %v", env)
	}
	// Critically: the hit must still be present in --json output —
	// pre-fix the flag was joined into the query and the FTS5 lookup
	// returned zero results.
	memList, _ := env["memories"].([]interface{})
	if len(memList) == 0 {
		t.Errorf("--json output returned 0 memories; flag was probably joined into the query. stdout=%s", stdoutJSON)
	}
}
