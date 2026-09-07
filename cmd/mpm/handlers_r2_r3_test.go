package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// R2 — --json for ls and show (and regression for the existing add/remember/
// memory add paths). R3 — --help short-circuit on every affected leaf
// handler. Both surfaces are exercised through real subprocess invocations
// so the full router + parseFlags + help-interception chain is covered.
// ---------------------------------------------------------------------------

// buildMpmForR2R3Test compiles the mpm binary into a per-test temp dir.
func buildMpmForR2R3Test(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "mpm")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", binPath, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mpm: %v\n%s", err, out)
	}
	return binPath
}

// runMpmR2R3 invokes the freshly built mpm with MPM_WORKSPACE set to a
// disposable dir and returns the parsed user-facing output (logs
// stripped) along with the exit code.
func runMpmR2R3(t *testing.T, bin, workspace string, args ...string) (string, int, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	cmd.Stdin, _ = os.Open(os.DevNull)
	combined, err := cmd.CombinedOutput()
	raw := string(combined)
	exit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			t.Fatalf("mpm %v: %v\n%s", args, err, raw)
		}
	}
	// Split combined into stdout + stderr approximation. The subprocess
	// test merges both, so we cannot perfectly partition; instead we
	// return the full combined output and let each test pattern-match.
	return filterR2R3Logs(raw), exit, raw
}

// filterR2R3Logs strips the slog log lines so the assertions focus on
// user-facing CLI output (recall results, hints, JSON envelopes, errors).
func filterR2R3Logs(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "time=") || strings.HasPrefix(line, "level=") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// findJSONEnvelope extracts the first JSON-shaped substring (object or
// array) from the user-facing output. We need this because some handlers
// emit a status banner before the JSON, which is fine on stdout but must
// not pollute the JSON envelope itself.
//
// Returns the JSON candidate and the index where it starts. Empty string
// means no JSON found.
//
// The naive "find the first [ or { and balance it" approach breaks on
// lines like "[mpm] scheduler: not running" that happen to contain a
// balanced bracket pair in a non-JSON context. To avoid that, we require
// the first non-whitespace character inside the bracket to be a JSON
// starter (object, array, string, number, true/false/null).
func findJSONEnvelope(out string) string {
	for i := 0; i < len(out); i++ {
		if out[i] != '{' && out[i] != '[' {
			continue
		}
		open, close := byte('{'), byte('}')
		if out[i] == '[' {
			open, close = '[', ']'
		}
		// Look ahead at the first non-whitespace character inside.
		j := i + 1
		for j < len(out) && (out[j] == ' ' || out[j] == '\t' || out[j] == '\n' || out[j] == '\r') {
			j++
		}
		if j >= len(out) {
			continue
		}
		switch out[j] {
		case '{', '[', '"', 't', 'f', 'n', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			// Looks JSON-shaped — try to balance.
			end := findMatchingBrace(out, i, open, close)
			if end > i {
				return out[i : end+1]
			}
		case close:
			// Empty object {} or empty array [] — single-character envelope.
			if j == i+1 {
				return out[i : j+1]
			}
		}
	}
	return ""
}

func findMatchingBrace(s string, start int, open, close byte) int {
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i
			}
		case '"':
			// Skip string contents (no nested escaping for our use case).
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				i++
			}
		}
	}
	return -1
}

// TestR2_AddJSON verifies that `mpm add --json <content>` emits valid
// JSON on stdout. The existing handleAdd shape is preserved.
func TestR2_AddJSON(t *testing.T) {
	bin := buildMpmForR2R3Test(t)
	workspace := t.TempDir()

	out, exit, _ := runMpmR2R3(t, bin, workspace, "add", "--json", "r2-add-test")
	if exit != 0 {
		t.Fatalf("mpm add --json exit=%d\n%s", exit, out)
	}
	envelope := findJSONEnvelope(out)
	if envelope == "" {
		t.Fatalf("no JSON envelope in output: %s", out)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(envelope), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, envelope)
	}
	if parsed["success"] != true {
		t.Errorf("expected success=true, got: %v", parsed["success"])
	}
	if _, ok := parsed["id"].(string); !ok {
		t.Errorf("expected id field as string, got: %v", parsed["id"])
	}
}

// TestR2_RememberJSON verifies the cognitive-verb alias `mpm remember --json`
// delegates to handleAdd and emits the same JSON envelope.
func TestR2_RememberJSON(t *testing.T) {
	bin := buildMpmForR2R3Test(t)
	workspace := t.TempDir()

	out, exit, _ := runMpmR2R3(t, bin, workspace, "remember", "--json", "r2-remember-test")
	if exit != 0 {
		t.Fatalf("mpm remember --json exit=%d\n%s", exit, out)
	}
	envelope := findJSONEnvelope(out)
	if envelope == "" {
		t.Fatalf("no JSON envelope in output: %s", out)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(envelope), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, envelope)
	}
	if parsed["success"] != true {
		t.Errorf("expected success=true, got: %v", parsed["success"])
	}
}

// TestR2_MemoryAddJSON verifies the `mpm memory add --json` path. This
// command has the richest JSON envelope (matches the mpm_memory save
// MCP contract).
func TestR2_MemoryAddJSON(t *testing.T) {
	bin := buildMpmForR2R3Test(t)
	workspace := t.TempDir()

	out, exit, _ := runMpmR2R3(t, bin, workspace, "memory", "add", "--json", "r2-memadd-test")
	if exit != 0 {
		t.Fatalf("mpm memory add --json exit=%d\n%s", exit, out)
	}
	envelope := findJSONEnvelope(out)
	if envelope == "" {
		t.Fatalf("no JSON envelope in output: %s", out)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(envelope), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, envelope)
	}
	if parsed["success"] != true {
		t.Errorf("expected success=true, got: %v", parsed["success"])
	}
	// The memory envelope includes a pointer; check the contract keys.
	if _, ok := parsed["id"].(string); !ok {
		t.Errorf("expected id field, got: %v", parsed["id"])
	}
	if _, ok := parsed["pointer"].(string); !ok {
		t.Errorf("expected pointer field, got: %v", parsed["pointer"])
	}
}

// TestR2_LsJSON verifies the new `mpm ls --json` path emits a JSON array.
func TestR2_LsJSON(t *testing.T) {
	bin := buildMpmForR2R3Test(t)
	workspace := t.TempDir()

	// Seed at least one memory so the array is non-trivial. Use the
	// --json path so we can extract the ID deterministically. The seed
	// must run in the same workspace as the assertions below.
	seedCmd := exec.Command(bin, "add", "--json", "r2-ls-seed-"+t.Name())
	seedCmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	seedOut, err := seedCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("seed: %v\n%s", err, seedOut)
	}
	id := jsonFieldFromShowStdout(t, string(seedOut), "id")
	if id == "" {
		t.Fatalf("could not extract seed id; seed output was: %s", seedOut)
	}

	// Now run `mpm ls --json --collection nothing-here-empty` to get a
	// reliably-empty array (filter excludes the seed). This validates
	// the empty-array case.
	out, exit, _ := runMpmR2R3(t, bin, workspace,
		"ls", "--collection", "nothing-here-empty", "--json")
	if exit != 0 {
		t.Fatalf("mpm ls --json exit=%d\n%s", exit, out)
	}
	envelope := findJSONEnvelope(out)
	if envelope == "" {
		t.Fatalf("no JSON envelope in output: %s", out)
	}
	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(envelope), &parsed); err != nil {
		t.Fatalf("invalid JSON array: %v\n%s", err, envelope)
	}
	if len(parsed) != 0 {
		t.Errorf("expected empty array for nonexistent collection, got %d rows", len(parsed))
	}

	// Now run again WITHOUT the filter; should get at least the seed.
	out, exit, _ = runMpmR2R3(t, bin, workspace, "ls", "--json")
	if exit != 0 {
		t.Fatalf("mpm ls --json (no filter) exit=%d\n%s", exit, out)
	}
	envelope = findJSONEnvelope(out)
	if envelope == "" {
		t.Fatalf("no JSON envelope in output: %s", out)
	}
	if err := json.Unmarshal([]byte(envelope), &parsed); err != nil {
		t.Fatalf("invalid JSON array: %v\n%s", err, envelope)
	}
	if len(parsed) == 0 {
		t.Errorf("expected at least 1 row (the seed), got empty array")
	}
}

// TestR2_ShowJSON verifies the new `mpm show --json <id>` path emits a
// single JSON object and does NOT consume --json as the ID.
func TestR2_ShowJSON(t *testing.T) {
	bin := buildMpmForR2R3Test(t)
	workspace := t.TempDir()

	// Seed a memory using the JSON path so we can extract the ID.
	// Must use the SAME workspace as the test assertions.
	seedCmd := exec.Command(bin, "add", "--json", "r2-show-seed-"+t.Name())
	seedCmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	seedOut, err := seedCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("seed: %v\n%s", err, seedOut)
	}
	id := jsonFieldFromShowStdout(t, string(seedOut), "id")
	if id == "" {
		t.Fatalf("could not extract seed id; seed output was: %s", seedOut)
	}

	out, exit, _ := runMpmR2R3(t, bin, workspace, "show", "--json", id)
	if exit != 0 {
		t.Fatalf("mpm show --json exit=%d\n%s", exit, out)
	}
	envelope := findJSONEnvelope(out)
	if envelope == "" {
		t.Fatalf("no JSON envelope in output: %s", out)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(envelope), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, envelope)
	}
	if got, _ := parsed["id"].(string); got != id {
		t.Errorf("expected id=%q in envelope, got %q", id, got)
	}

	// Regression: --json MUST NOT be consumed as the ID. The original
	// bug was `mpm show --json` looking up memory "--json".
	out, exit, _ = runMpmR2R3(t, bin, workspace, "show", "--json", "definitely-not-an-id-12345")
	if exit == 0 {
		t.Errorf("expected non-zero exit for missing ID, got 0:\n%s", out)
	}
	if !strings.Contains(out, "Memory not found") {
		t.Errorf("expected 'Memory not found' diagnostic, got: %s", out)
	}
}

// jsonFieldFromShowStdout extracts a top-level field from the first
// JSON object in `mpm add`'s stdout. handleAdd prints a JSON envelope
// at the end, so this works against its output.
func jsonFieldFromShowStdout(t *testing.T, s, field string) string {
	t.Helper()
	envelope := findJSONEnvelope(s)
	if envelope == "" {
		return ""
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(envelope), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, envelope)
	}
	v, _ := parsed[field].(string)
	return v
}

// ---------------------------------------------------------------------------
// R3 — --help short-circuit on memory add (the user-reported bug) and
// its affected siblings. The test asserts:
//
//   - prints help
//   - does not mutate state
//   - does not invoke embedding
//   - does not require API credentials
//   - does not emit unrelated success/error lines
//   - returns exit 0
// ---------------------------------------------------------------------------

// TestR3_HelpNoSideEffects is a parametric test that runs `<cmd> --help`
// against a fresh workspace and asserts every property of a side-effect-
// free help path. Each case name maps to the user-facing command from
// the manual validation report.
func TestR3_HelpNoSideEffects(t *testing.T) {
	bin := buildMpmForR2R3Test(t)
	workspace := t.TempDir()

	cases := []struct {
		name string
		args []string
	}{
		{"memory add", []string{"memory", "add", "--help"}},
		{"memory search", []string{"memory", "search", "--help"}},
		{"memory list", []string{"memory", "list", "--help"}},
		{"memory show", []string{"memory", "show", "--help"}},
		{"lesson add", []string{"lesson", "add", "--help"}},
		{"session add", []string{"session", "add", "--help"}},
		{"topic add", []string{"topic", "add", "--help"}},
		{"reference add", []string{"reference", "add", "--help"}},
		{"work status", []string{"work", "status", "--help"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, exit, _ := runMpmR2R3(t, bin, workspace, tc.args...)
			if exit != 0 {
				t.Errorf("help should exit 0, got %d\n%s", exit, out)
			}
			// 1. prints help (look for the namespace header)
			if !strings.Contains(out, "Usage:") {
				t.Errorf("expected help to print 'Usage:', got: %s", out)
			}
			// 2. does not mutate state (no "added"/"stored"/"Memory added"
			//    success lines). These strings are exactly the
			//    contamination the original bug produced.
			//
			// Note: "Working Context" intentionally stays in the
			// negative-pattern list as a mutation marker for the OTHER
			// commands; the work/help subcommand is excluded via a
			// separate check below.
			mutations := []string{
				"Memory added:",
				"Lesson added",
				"Session added",
				"Topic added:",
				"File not found",
				"Recent ",
				"Found 1 memories",
				"No memories found",
			}
			// For non-work commands, "Working Context" would be a
			// sign the renderer for handleWorkStatus leaked through.
			if tc.name != "work status" {
				mutations = append(mutations, "Working Context")
			}
			for _, m := range mutations {
				if strings.Contains(out, m) {
					t.Errorf("help must not emit %q; got: %s", m, out)
				}
			}
			// 3. does not invoke embedding or require API credentials.
			//    The original bug surfaced as "Poison phrases seeded"
			//    plus an embedding attempt; both must be absent.
			apiHints := []string{
				"Poison phrases seeded",
				"embedding",
				"API key",
				"provider",
				"No such file",
			}
			for _, m := range apiHints {
				if strings.Contains(out, m) {
					t.Errorf("help must not trigger %q; got: %s", m, out)
				}
			}
		})
	}
}

// TestR3_HelpPreservesWorkItemHelp guards against regressing the
// RECOMMENDED 12 fix: `mpm work item --help` MUST continue to print the
// work-item-specific help page (not fall back to the work-namespace
// page). The R3 fix was applied at the leaf-handler level rather than
// the dispatcher level specifically to preserve this behaviour.
func TestR3_HelpPreservesWorkItemHelp(t *testing.T) {
	bin := buildMpmForR2R3Test(t)
	workspace := t.TempDir()

	out, exit, _ := runMpmR2R3(t, bin, workspace, "work", "item", "--help")
	if exit != 0 {
		t.Fatalf("mpm work item --help exit=%d\n%s", exit, out)
	}
	// work-item help lists the item subcommands (create, list, show, ...)
	if !strings.Contains(out, "create") || !strings.Contains(out, "list") {
		t.Errorf("expected work-item help with create/list subcommands, got: %s", out)
	}
	// It must NOT contain the scratchpad-level work help markers.
	if strings.Contains(out, "Expose the current Working Context") {
		t.Errorf("work item --help regressed to work-level help:\n%s", out)
	}
}

// TestR2_LF1StillIntact pins the LF1 fix in light of the new show --json
// flag. `mpm show --json <id>` and `mpm show <id> --json` must both
// parse correctly. The reorderFlagsBeforePositionals pattern was used
// in handleAdd; this test guards against accidentally breaking the
// equivalent parsing path in handleShow.
func TestR2_LF1StillIntactForShow(t *testing.T) {
	bin := buildMpmForR2R3Test(t)
	workspace := t.TempDir()

	seedCmd := exec.Command(bin, "add", "--json", "r2-lf1-seed-"+t.Name())
	seedCmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	seedOut, err := seedCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("seed: %v\n%s", err, seedOut)
	}
	id := jsonFieldFromShowStdout(t, string(seedOut), "id")
	if id == "" {
		t.Fatalf("could not extract seed id from: %s", seedOut)
	}

	// Flag before ID (the natural form).
	out1, exit1, _ := runMpmR2R3(t, bin, workspace, "show", "--json", id)
	if exit1 != 0 {
		t.Errorf("--json before id: exit=%d\n%s", exit1, out1)
	}
	if findJSONEnvelope(out1) == "" {
		t.Errorf("--json before id: expected JSON envelope, got: %s", out1)
	}

	// Flag after ID (the LF1 case — must also work).
	out2, exit2, _ := runMpmR2R3(t, bin, workspace, "show", id, "--json")
	if exit2 != 0 {
		t.Errorf("--json after id: exit=%d\n%s", exit2, out2)
	}
	if findJSONEnvelope(out2) == "" {
		t.Errorf("--json after id: expected JSON envelope, got: %s", out2)
	}
}
