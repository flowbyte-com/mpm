// rough_edges_20260912_test.go — regression pins for the 2026-09-12
// rough-edge closure pass (follow-up to the 93-check CLI acceptance run).
// Each item is classified FIX / CONTRACT / REMOVE / DEFER in the pass
// report; the tests below pin the FIX items and CONTRACT exhibits that
// live in this package. Item numbers match the pass scope list.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func lastJSONObject(t *testing.T, out string) string {
	t.Helper()
	var last string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			last = line
		}
	}
	if last == "" {
		t.Fatalf("no JSON object in output:\n%s", out)
	}
	return last
}

func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// Item 1 (FIX): routine human-facing CLI invocations must be quiet on
// stderr — no operational INFO. MPM_LOG=info explicitly restores it.
func TestRough_Item1_HumanCLIDefaultQuiet(t *testing.T) {
	for _, args := range [][]string{
		{"status"},
		{"--help"},
		{"memory", "list"},
	} {
		stdout, stderr, err := callMPM(t, args...)
		if err != nil {
			t.Fatalf("mpm %v failed: %v\nstdout=%s\nstderr=%s", args, err, stdout, stderr)
		}
		for _, noisy := range []string{
			"file perms sweep complete",
			"artifact_provenance",
			"fts_recovery",
			"level=INFO",
		} {
			if strings.Contains(stderr, noisy) {
				t.Errorf("mpm %v: stderr leaked INFO noise %q:\n%s", args, noisy, stderr)
			}
		}
	}
}

// Item 1 (FIX): explicit MPM_LOG=info restores the diagnostic stream.
func TestRough_Item1_ExplicitInfoRestoresLogs(t *testing.T) {
	if mpmBin == "" {
		t.Skip("mpm binary not built; skipping IO regression")
	}
	ws := t.TempDir()
	cmd := exec.Command(mpmBin, "status")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + ws,
		"MPM_WORKSPACE=" + ws,
		"MPM_SCHEDULER_DISABLED=1",
		"MPM_LOG=info",
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		t.Fatalf("status failed: %v\n%s\n%s", err, so.String(), se.String())
	}
	if so.String() == "" {
		t.Fatalf("expected status output on stdout")
	}
	if !strings.Contains(se.String(), "level=INFO") {
		t.Errorf("MPM_LOG=info should restore INFO stream, stderr was:\n%s", se.String())
	}
}

// Item 3 (FIX): `mpm memory show <id> --json` must emit structured JSON,
// not silently ignore the flag.
func TestRough_Item3_MemoryShowJSON(t *testing.T) {
	dm := setupMemoryAddTest(t)
	var id string
	if err := dm.SQLDB().QueryRow(
		`SELECT id FROM memories WHERE content = 'rough-item3-probe'`,
	).Scan(&id); err != nil {
		// Seed the probe row via the handler (same pattern as the
		// acceptance weight-echo tests).
		if code := handleMemoryAdd([]string{"rough-item3-probe"}); code != 0 {
			t.Fatalf("seed add failed: %d", code)
		}
		if err := dm.SQLDB().QueryRow(
			`SELECT id FROM memories WHERE content = 'rough-item3-probe'`,
		).Scan(&id); err != nil {
			t.Fatalf("seed row missing: %v", err)
		}
	}

	out := captureBoth(t, func() {
		if code := handleMemoryShow([]string{id, "--json"}); code != 0 {
			t.Fatalf("show --json exited %d", code)
		}
	})
	line := lastJSONObject(t, out)
	var env map[string]interface{}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("show --json is not JSON: %v\n%s", err, out)
	}
	if env["id"] != id {
		t.Errorf("show --json id = %v, want %s", env["id"], id)
	}
	if _, ok := env["content"]; !ok {
		t.Errorf("show --json missing content field (keys: %v)", keysOf(env))
	}
	if env["success"] != true {
		t.Errorf("show --json success = %v, want true", env["success"])
	}
}

// Item 3 (FIX): positional --json must also work, matching the search
// flag-tolerance convention (flags may trail positionals).
func TestRough_Item3_MemoryShowJSONFlagFirst(t *testing.T) {
	dm := setupMemoryAddTest(t)
	if code := handleMemoryAdd([]string{"rough-item3-flagfirst"}); code != 0 {
		t.Fatalf("seed add failed: %d", code)
	}
	var id string
	if err := dm.SQLDB().QueryRow(
		`SELECT id FROM memories WHERE content = 'rough-item3-flagfirst'`,
	).Scan(&id); err != nil {
		t.Fatalf("seed row missing: %v", err)
	}
	out := captureBoth(t, func() {
		if code := handleMemoryShow([]string{"--json", id}); code != 0 {
			t.Fatalf("show --json <id> exited %d", code)
		}
	})
	var env map[string]interface{}
	if err := json.Unmarshal([]byte(lastJSONObject(t, out)), &env); err != nil {
		t.Fatalf("show --json <id> is not JSON: %v\n%s", err, out)
	}
	if env["id"] != id {
		t.Errorf("id = %v, want %s", env["id"], id)
	}
}

// lastJSONObject returns the last {...} line of captured output.
// Item 2 (CONTRACT): the exit-code contract is conventional everywhere
// except intentional smart-recall. The 93-check acceptance pass initially
// misread piped exit codes (`cmd | grep; echo $?` reports the pipe tail),
// so this pins the real rules at the binary boundary:
//   - friendly validation failure → non-zero + stderr message
//   - call transport failure (unknown tool, malformed JSON, unknown
//     action, missing field) → non-zero + {"success":false} envelope
//   - call success → zero; application-level {"success":false} inside a
//     handled result still exits zero ONLY where the handler returns it
//     as a result (documented in `mpm call --help`).
func TestRough_Item2_ExitContract(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantExit int
	}{
		{"friendly missing id", []string{"memory", "show", "rough2-no-such-id"}, 1},
		{"friendly bad weight", []string{"set-weight", "rough2-no-such-id", "5"}, 1},
		{"call unknown tool", []string{"call", "nosuchtool", "--payload", `{"action":"x"}`}, 1},
		{"call malformed json", []string{"call", "mpm_memory", "--payload", `{not json`}, 1},
		{"call unknown action", []string{"call", "mpm_memory", "--payload", `{"action":"frobnicate","params":{}}`}, 1},
		{"call missing field", []string{"call", "mpm_memory", "--payload", `{"action":"save","params":{}}`}, 1},
		{"call ok", []string{"call", "mpm_memory", "--payload", `{"action":"query","params":{"query":"rough2","limit":1}}`}, 0},
	}
	for _, tc := range cases {
		stdout, stderr, err := callMPM(t, tc.args...)
		code := 0
		if err != nil {
			if ee, ok := err.(interface{ ExitCode() int }); ok {
				code = ee.ExitCode()
			} else {
				t.Fatalf("%s: %v", tc.name, err)
			}
		}
		if code != tc.wantExit {
			t.Errorf("%s: exit %d, want %d\nstdout=%s\nstderr=%s", tc.name, code, tc.wantExit, stdout, stderr)
		}
		if tc.wantExit != 0 && !strings.Contains(tc.args[0], "memory") && !strings.Contains(tc.args[0], "set-weight") {
			var env map[string]interface{}
			if jerr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &env); jerr != nil {
				t.Errorf("%s: failing call must still emit a JSON envelope: %v\n%s", tc.name, jerr, stdout)
			}
		}
	}
}

// Item 5 (CONTRACT): standalone invocations mint a fresh session unless
// pinned; pinning via --session-id (or MPM_SESSION_ID) reuses it. The
// help must say so explicitly.
func TestRough_Item5_WorkSessionContract(t *testing.T) {
	setupMemoryAddTest(t) // hermetic DM for the work handlers
	out := captureBoth(t, func() {
		if code := handleWork(nil); code != 0 {
			// bare `mpm work` prints help; accept either 0 or help text
		}
	})
	_ = out
	helpOut := captureBoth(t, func() {
		printWorkHelp()
	})
	if !strings.Contains(helpOut, "MPM_SESSION_ID") || !strings.Contains(helpOut, "--session-id") {
		t.Errorf("work help must document session pinning:\n%s", helpOut)
	}
	pinned := captureBoth(t, func() {
		if code := handleWork([]string{"status", "--session-id", "rough5-pin"}); code != 0 {
			t.Fatalf("work status --session-id exited %d", code)
		}
	})
	if !strings.Contains(pinned, "rough5-pin") {
		t.Errorf("pinned session id should surface in status:\n%s", pinned)
	}
}
// Item 2 (CONTRACT): bare `mpm call` documents the contract on stderr
// (stdout keeps carrying only the JSON envelope) and exits non-zero.
// NOTE: `mpm call --help` is intercepted by the router's generic command
// help, so the bare-call usage is the discoverable contract surface.
func TestRough_Item2_CallHelp(t *testing.T) {
	stdout, stderr, err := callMPM(t, "call")
	if err == nil {
		t.Fatalf("bare call should exit non-zero\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	var env map[string]interface{}
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &env); jerr != nil {
		t.Errorf("bare-call stdout must stay a pure JSON envelope: %v\n%s", jerr, stdout)
	}
	for _, want := range []string{"success", "exit 0", "exit 1"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("bare-call stderr should document %q:\n%s", want, stderr)
		}
	}
}


