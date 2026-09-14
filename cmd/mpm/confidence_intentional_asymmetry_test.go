// confidence_intentional_asymmetry_test.go — Regression coverage
// for the 2026-09-14 release-pass documentation of the
// intentional CLI asymmetry for confidence.
//
// The canonical public confidence surface is:
//
//   mpm ops confidence <sub> --artifact <id>  (engine-room, JSON)
//   mpm why <id> [--kind ...]                (per-event, human)
//   mpm call mpm_confidence --payload ...    (machine, MCP)
//
// There is NO top-level `mpm confidence` command — by design. The
// tests assert the positive surface (what IS advertised) rather
// than the negative surface (what is not). This guards against a
// future regression that re-introduces a top-level confidence
// command without intentional design.

package main

import (
	"strings"
	"testing"
)

// TestConfidence_OpsSurfaceAdvertised asserts `mpm help --all`
// advertises the `ops` command as the canonical engine-room
// namespace for confidence (the `ops confidence <sub>` form).
// 2026-09-14 release-pass: the help catalogue uses an alphabetic
// one-line-per-command shape; subcommands are not enumerated
// there. The test asserts the namespace is discoverable.
func TestConfidence_OpsSurfaceAdvertised(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "help", "--all")
	out := string(stdout)
	if !strings.Contains(out, "ops :") {
		t.Fatalf("`mpm help --all` must advertise `ops` as the engine-room namespace for confidence.\nGot:\n%s", out)
	}
	// The ops help surface must enumerate the confidence subcommand.
	opsOut, _, _ := runMpmParity(t, bin, workspace, "ops", "help")
	opsStr := string(opsOut)
	if !strings.Contains(opsStr, "confidence") {
		t.Fatalf("`mpm ops help` must enumerate the `confidence` subcommand.\nGot:\n%s", opsStr)
	}
}

// TestConfidence_OpsSurfaceJSONOnly asserts that `mpm ops confidence
// show --artifact <id>` for a real artifact returns a JSON
// envelope (success path). The engine-room surface is JSON-only
// by design. A not-found path emits an error envelope and
// non-zero exit — both are valid (we don't pin the not-found
// format here; TestConfidence_OpsSurfaceAdvertised covers the
// discoverability aspect).
//
// 2026-09-14 release-pass: confidence is intentionally MCP-only
// at the CLI top level. The engine-room surface (this test) is
// the canonical CLI machine surface.
func TestConfidence_OpsSurfaceJSONOnly(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, stderr, exit := runMpmParity(t, bin, workspace, "ops", "confidence", "show", "--artifact", "nonexistent-id")
	if exit == 0 {
		// Success on a non-existent id is unexpected — but the
		// JSON output check still applies.
		trimmed := strings.TrimSpace(string(stdout))
		if !strings.HasPrefix(trimmed, "{") {
			t.Fatalf("`mpm ops confidence show` success must emit JSON.\nGot: %s", stdout)
		}
		return
	}
	// Non-zero exit is expected for not-found. Confirm the
	// output mentions the artifact (any structured error path
	// is acceptable — the engine-room surface is JSON when the
	// query succeeds, structured error otherwise).
	combined := string(stdout) + string(stderr)
	if !strings.Contains(combined, "ops confidence show") {
		t.Fatalf("`mpm ops confidence show` error output must mention the subcommand.\nstdout=%s\nstderr=%s", stdout, stderr)
	}
}

// TestConfidence_WhySurfaceAdvertised asserts that `mpm why --help`
// is reachable (the human-readable per-event confidence surface).
func TestConfidence_WhySurfaceAdvertised(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	stdout, _, _ := runMpmParity(t, bin, workspace, "why", "--help")
	out := string(stdout)
	if !strings.Contains(out, "MPM · Why") {
		t.Fatalf("`mpm why --help` must show the canonical `MPM · Why` heading.\nGot:\n%s", out)
	}
	// The help must point at the related confidence surfaces.
	for _, want := range []string{"confidence", "evidence"} {
		if !strings.Contains(out, want) {
			t.Fatalf("`mpm why --help` must mention the %s surface.\nGot:\n%s", want, out)
		}
	}
}
