package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The canonical store location is only canonical if every production
// consumer agrees on it.
//
// The telemetry store's path was decided TWICE — by the collector and
// by docs/SPEC.md §6.6 — and the two disagreed. The code won, because
// the code is where the frames land; but the spec went on naming the
// other path, and a THIRD consumer (the critic's telemetry hunt)
// followed the spec and was therefore inert.
//
// So the location is now asserted from three independent directions:
// the resolver itself, the systemd unit that runs the collector in
// production, and the specification that documents it. These tests
// pin all three against each other, so a future edit that moves one of
// them fails here instead of silently in production.

// repoRoot returns the repository root from this package's directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}
	return root
}

// unitEnv reads an Environment= assignment from a systemd unit file.
func unitEnv(t *testing.T, unit, key string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "contrib", "systemd", unit))
	if err != nil {
		t.Fatalf("read %s: %v", unit, err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Environment=") {
			continue
		}
		kv := strings.TrimPrefix(line, "Environment=")
		if strings.HasPrefix(kv, key+"=") {
			return strings.Trim(strings.TrimPrefix(kv, key+"="), `"`)
		}
	}
	t.Fatalf("%s does not set %s", unit, key)
	return ""
}

// TestCanonicalStorePath_MatchesTheSystemdUnit pins that the unit that
// runs the collector in production points at the resolver's canonical
// path.
//
// The unit is the production consumer: it is what the collector runs
// with on a real machine, and it is what an operator reads when they
// want to know where the ledger lives. If the resolver and the unit
// disagree, the observer looks where the collector does not write —
// which is exactly what happened to the critic's hunt.
func TestCanonicalStorePath_MatchesTheSystemdUnit(t *testing.T) {
	unit := "mpm-telemetry.service.user"

	raw := unitEnv(t, unit, "MPM_TELEMETRY_DB")
	rawWS := unitEnv(t, unit, "MPM_WORKSPACE")

	// The unit writes %h for the user's home; expand it against a
	// synthetic home so the comparison is exact. The workspace is
	// $HOME/.mpm — the canonical runtime root.
	home := "/home/example"
	expand := func(s string) string {
		return strings.ReplaceAll(s, "%h", home)
	}
	workspace := expand(rawWS)
	expanded := expand(raw)

	want := StorePath(workspace)
	if expanded != want {
		t.Errorf("the systemd unit's MPM_TELEMETRY_DB resolves to %q, "+
			"but the canonical path for its MPM_WORKSPACE (%q) is %q — the "+
			"collector would write where the observer does not look",
			expanded, workspace, want)
	}
}

// TestCanonicalStorePath_MatchesTheSpec pins that the specification
// names the canonical path.
//
// This is the assertion that would have caught the original divergence:
// the spec and the code disagreed for two months, and the only
// consumer that read the spec was the one that broke.
func TestCanonicalStorePath_MatchesTheSpec(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "SPEC.md"))
	if err != nil {
		t.Fatalf("read SPEC.md: %v", err)
	}
	spec := string(body)

	if !strings.Contains(spec, "$MPM_WORKSPACE/src/db/telemetry.db") {
		t.Error("SPEC.md does not name the canonical telemetry store path " +
			"($MPM_WORKSPACE/src/db/telemetry.db)")
	}
	if strings.Contains(spec, "$MPM_WORKSPACE/telemetry.db") {
		t.Error("SPEC.md still names $MPM_WORKSPACE/telemetry.db — a path " +
			"nothing writes to")
	}
}

// TestCanonicalSocketPath_MatchesTheSpec pins the socket path, the
// second drift in the same clause.
//
// The code unified on `runtime/` in commit 664081e; the spec said
// `run/`. An operator reading the spec would have looked in the wrong
// place for the socket — which is how every `mpm-telemetry ping` failed
// on a default install until someone noticed.
func TestCanonicalSocketPath_MatchesTheSpec(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "SPEC.md"))
	if err != nil {
		t.Fatalf("read SPEC.md: %v", err)
	}
	spec := string(body)

	if !strings.Contains(spec, "$MPM_WORKSPACE/runtime/mpm-telemetry.sock") {
		t.Error("SPEC.md does not name the canonical telemetry socket path " +
			"($MPM_WORKSPACE/runtime/mpm-telemetry.sock)")
	}
	if strings.Contains(spec, "$MPM_WORKSPACE/run/mpm-telemetry.sock") {
		t.Error("SPEC.md still names $MPM_WORKSPACE/run/mpm-telemetry.sock — " +
			"a path the daemon does not bind")
	}
}

// TestCanonicalStorePath_MatchesTheSystemdSocket pins that the unit and
// the spec agree on the socket, so the two drift checks are consistent.
func TestCanonicalStorePath_MatchesTheSystemdSocket(t *testing.T) {
	raw := unitEnv(t, "mpm-telemetry.service.user", "MPM_TELEMETRY_SOCKET")
	rawWS := unitEnv(t, "mpm-telemetry.service.user", "MPM_WORKSPACE")
	home := "/home/example"
	expand := func(s string) string {
		return strings.ReplaceAll(s, "%h", home)
	}
	workspace := expand(rawWS)
	want := filepath.Join(workspace, "runtime", "mpm-telemetry.sock")
	if got := expand(raw); got != want {
		t.Errorf("the systemd unit's MPM_TELEMETRY_SOCKET resolves to %q, want %q",
			got, want)
	}
}
