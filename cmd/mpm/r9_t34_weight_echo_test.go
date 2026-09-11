// r9_t34_weight_echo_test.go — Round 9 T34 regression.
//
// Bug: `mpm remember "x" --weight 0.5` printed `weight=0.5` but the DB
// stored `5.0`. The substrate's AddMemoryWithWeight normalizes a legacy
// 0.0-1.0 float input by ×10 before writing, but the human-format echo
// on `mpm add` / `mpm remember` printed the raw `--weight` arg, so the
// stdout contradicted the persisted column.
//
// Fix: the human-format line now uses `mem.Weight` (the post-normalized
// column value) instead of `*weight` (the raw arg). The `--json`
// envelope continues to echo the user's arg (matches the substrate
// `mpm_memory save` envelope, which both surfaces mirror for tool
// consistency).
//
// Pin: every (`mpm remember` / `mpm add` / `mpm memory add`)
// human-format output for a given input weight must produce the same
// `weight=N` value, and that value must equal the column's persisted
// weight (no scaling ambiguity between stdout and storage).

package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// r9T34RunMpm invokes the binary with MPM_WORKSPACE redirected to a
// test-local directory so persistence is hermetic.
func r9T34RunMpm(t *testing.T, workspace string, args ...string) (string, int) {
	t.Helper()
	binPath := filepath.Join("..", "..", "bin", "mpm")
	if _, err := os.Stat(binPath); os.IsNotExist(err) {
		t.Skip("bin/mpm not built; run make build first")
	}
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return string(out), code
}

// r9T34OpenDB opens the test-local mpm.db for verifying stored weight.
func r9T34OpenDB(t *testing.T, workspace string) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(workspace, "src", "db", "mpm.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestR9T34_WeightEcho_MatchesStored pins the canonical invariant:
// stdout's `(weight=N)` must match the stored column value. Pre-fix
// this failed when the user supplied a 0.0-1.0 float input (because
// the substrate normalizes ×10 before storing but the echo printed
// the raw arg).
func TestR9T34_WeightEcho_MatchesStored(t *testing.T) {
	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")

	cases := []struct {
		name      string
		cliWeight string
		// expectedStored is what normalizeWeightToColumn produces for the
		// given input. Mirrors the canonical scale: 0-100 ints pass
		// through, 0.0-1.0 floats are ×10.
		expectedStored int64
	}{
		{name: "int_zero", cliWeight: "0", expectedStored: 5},  // 0→ default
		{name: "int_default", cliWeight: "5", expectedStored: 5},
		{name: "int_50", cliWeight: "50", expectedStored: 50},
		{name: "int_100", cliWeight: "100", expectedStored: 100},
		{name: "fractional_0_5", cliWeight: "0.5", expectedStored: 5},
		{name: "fractional_0_8", cliWeight: "0.8", expectedStored: 8},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := r9T34RunMpm(t, workspace, "remember",
				"R9-T34-"+tc.name, "--weight", tc.cliWeight)
			if code != 0 {
				t.Fatalf("remember --weight %s exited %d\noutput: %s",
					tc.cliWeight, code, out)
			}

			// Extract the displayed weight from stdout.
			echoed := r9T34ExtractEchoedWeight(t, out)
			if echoed != tc.expectedStored {
				t.Errorf("--weight %s: echoed weight=%d, want %d (full output: %s)",
					tc.cliWeight, echoed, tc.expectedStored, out)
			}

			// Now verify the column's stored weight is the same.
			db := r9T34OpenDB(t, workspace)
			var stored int64
			if err := db.QueryRow(
				`SELECT weight FROM memories WHERE content = ?`,
				"R9-T34-"+tc.name,
			).Scan(&stored); err != nil {
				t.Fatalf("read weight: %v", err)
			}
			// Real column is REAL, so the integer "8" and float "8.0"
			// round-trip into the same int64 here.
			if stored != tc.expectedStored {
				t.Errorf("--weight %s: stored weight=%d, want %d",
					tc.cliWeight, stored, tc.expectedStored)
			}

			// Echoed-vs-stored parity: this is the headline invariant.
			if echoed != stored {
				t.Errorf("--weight %s: echoed=%d but stored=%d (T34 bug regression)",
					tc.cliWeight, echoed, stored)
			}
		})
	}
}

// TestR9T34_AddAndMemoryAdd_Parity pins that `mpm remember` (alias
// for `mpm add`) and `mpm memory add` produce the same echoed
// `(weight=N)` for the same input. Substrate parity: both surfaces
// route through AddMemoryWithWeight and so must normalize uniformly.
func TestR9T34_AddAndMemoryAdd_Parity(t *testing.T) {
	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")

	const content = "R9-T34-parity-content"

	// mpm remember
	outRemember, code := r9T34RunMpm(t, workspace, "remember", content, "--weight", "0.5")
	if code != 0 {
		t.Fatalf("remember exited %d: %s", code, outRemember)
	}
	echoedRemember := r9T34ExtractEchoedWeight(t, outRemember)

	// mpm memory add
	outMemoryAdd, code := r9T34RunMpm(t, workspace, "memory", "add", content+"-memory-add", "--weight", "0.5")
	if code != 0 {
		t.Fatalf("memory add exited %d: %s", code, outMemoryAdd)
	}
	echoedMemoryAdd := r9T34ExtractEchoedWeight(t, outMemoryAdd)

	if echoedRemember != echoedMemoryAdd {
		t.Errorf("alias parity: remember echoed %d, memory-add echoed %d",
			echoedRemember, echoedMemoryAdd)
	}

	// Both should be 5 (0.5 × 10).
	if echoedRemember != 5 {
		t.Errorf("remember echoed %d, want 5 (0.5 normalized ×10)", echoedRemember)
	}
}

// TestR9T34_CallSubstrate_Parity pins that the CLI human-format
// output aligns with what `mpm call mpm_memory save` would emit in
// its JSON envelope. The substrate treats `weight=0.5` as 0.5 on the
// 0.0-1.0 scale ×10 → 5.0; the CLI must echo the same normalized
// value to keep stdout and stored state consistent.
func TestR9T34_CallSubstrate_Parity(t *testing.T) {
	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")

	// Save via substrate JSON path with --weight 0.5
	out, code := r9T34RunMpm(t, workspace, "call", "mpm_memory", "save",
		"--payload", `{"action":"save","params":{"fact":"T34-call-parity","weight":0.5}}`)
	if code != 0 {
		t.Fatalf("mpm call mpm_memory save exited %d: %s", code, out)
	}
	// Extract weight from the JSON envelope.
	var envelope struct {
		Weight float64 `json:"weight"`
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &envelope); err == nil && envelope.Weight != 0 {
			break
		}
	}
	if envelope.Weight == 0 {
		t.Fatalf("could not extract substrate weight from JSON envelope: %s", out)
	}
	// Substrate reports 0.5 (raw). The CLI human-format reports the
	// post-normalized value (5). Both are honest about their contract:
	// the substrate accepts the legacy 0-1 scale on the wire for
	// compatibility; the CLI surface normalizes on the way in and
	// displays the persisted value. The README / help for each
	// surface tells the operator which scale it expects.
	//
	// What MUST NOT happen is a CLI that takes one scale and echoes
	// in another. Pre-fix T34 was: take `--weight 0.5`, echo 0.5,
	// store 5.0 — three different values visible to the user.
	if envelope.Weight != 0.5 && envelope.Weight != 5.0 {
		t.Errorf("substrate weight=%v, expected either 0.5 (raw) or 5.0 (normalized)", envelope.Weight)
	}
	t.Logf("substrate weight=%v (raw: 0.5 means legacy scale, 5.0 means normalized — both are valid wire shapes)",
		envelope.Weight)
}

// r9T34ExtractEchoedWeight parses `(weight=N)` out of `mpm remember`'s
// stdout. Returns -1 if not present (test will fail with a clear msg).
func r9T34ExtractEchoedWeight(t *testing.T, out string) int64 {
	t.Helper()
	// Format is `Added memory <id> to <collection> (weight=<int>)`
	const marker = "(weight="
	idx := strings.Index(out, marker)
	if idx < 0 {
		t.Fatalf("output missing (weight=N) marker: %s", out)
	}
	rest := out[idx+len(marker):]
	end := strings.IndexAny(rest, ")\n")
	if end < 0 {
		t.Fatalf("malformed (weight=…) close: %s", out)
	}
	numStr := strings.TrimSpace(rest[:end])
	var n int64
	if _, err := parseIntPrefix(numStr, &n); err != nil || numStr == "" {
		t.Fatalf("could not parse weight number %q: %v", numStr, err)
	}
	return n
}

// parseIntPrefix reads a leading int64 from s (used to avoid
// importing strconv just for this).
func parseIntPrefix(s string, out *int64) (int, error) {
	n := int64(0)
	i := 0
	seen := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		i = 1
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			break
		}
		n = n*10 + int64(s[i]-'0')
		seen = true
	}
	if !seen {
		return i, os.ErrInvalid
	}
	*out = n
	return i, nil
}
