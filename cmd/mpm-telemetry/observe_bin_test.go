package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `mpm-telemetry observe` shells out to `mpm call mpm_provenance` and
// `mpm call mpm_lessons` for cross-DB lookups. Its `--mpm` flag defaulted to
// the bare string "mpm", so an unattended observe run joined against
// whichever mpm PATH offered. Under a user unit that PATH belongs to the
// systemd manager, not the operator's shell, so the counts were plausible and
// wrong at the same time.
//
// Resolution is lazy: `observe --dry-run` prints findings without saving a
// lesson, and on a quiet store the hunt may never shell out at all. Neither
// case should fail because a binary could not be resolved for a subprocess
// that was never going to run.

func writeFakeMPMBin(t *testing.T, dir, name, logPath string) string {
	t.Helper()
	q := "'" + strings.ReplaceAll(logPath, "'", `'\''`) + "'"
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$0 $*\" >> " + q + "\n" +
		"if [ \"$2\" = \"mpm_provenance\" ]; then printf '%s' '{\"count\":0}'; fi\n" +
		"exit 0\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake mpm: %v", err)
	}
	return path
}

func writeHostileMPMBin(t *testing.T, dir, logPath string) {
	t.Helper()
	q := "'" + strings.ReplaceAll(logPath, "'", `'\''`) + "'"
	path := filepath.Join(dir, "mpm")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho HOSTILE >> "+q+"\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write hostile mpm: %v", err)
	}
}

func readCallLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

// TestObserve_ExplicitMpmFlagWins covers requirement K.
func TestObserve_ExplicitMpmFlagWins(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "explicit.log")
	explicit := writeFakeMPMBin(t, dir, "explicit-mpm", log)

	// A different binary is configured in the environment; the flag wins.
	t.Setenv("MPM_BIN", writeFakeMPMBin(t, dir, "env-mpm", filepath.Join(dir, "env.log")))

	l := &lazyMPMBin{explicit: explicit}
	out, err := runMpmCall(context.Background(), l, "mpm_provenance", []byte(`{}`))
	if err != nil {
		t.Fatalf("runMpmCall: %v", err)
	}
	if !strings.Contains(string(out), `"count"`) {
		t.Errorf("output = %q, want the fake binary's JSON response", out)
	}

	got := readCallLog(t, log)
	if !strings.Contains(got, explicit+" call mpm_provenance") {
		t.Errorf("call log = %q, want it to name the --mpm binary %q", got, explicit)
	}
	if envLog := readCallLog(t, filepath.Join(dir, "env.log")); envLog != "" {
		t.Errorf("MPM_BIN binary ran despite an explicit --mpm: %q", envLog)
	}
}

// TestObserve_DefaultResolverUsesMPMBin covers requirement L for the case
// where --mpm is absent.
func TestObserve_DefaultResolverUsesMPMBin(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "env.log")
	envBin := writeFakeMPMBin(t, dir, "env-mpm", log)
	t.Setenv("MPM_BIN", envBin)

	l := &lazyMPMBin{}
	if _, err := runMpmCall(context.Background(), l, "mpm_provenance", []byte(`{}`)); err != nil {
		t.Fatalf("runMpmCall: %v", err)
	}

	if got := readCallLog(t, log); !strings.Contains(got, envBin+" call mpm_provenance") {
		t.Errorf("call log = %q, want it to name MPM_BIN %q", got, envBin)
	}
}

// TestObserve_HostilePathIsIgnored covers requirement M.
func TestObserve_HostilePathIsIgnored(t *testing.T) {
	dir := t.TempDir()
	goodLog := filepath.Join(dir, "good.log")
	good := writeFakeMPMBin(t, dir, "mpm", goodLog)
	t.Setenv("MPM_BIN", good)

	hostileDir := t.TempDir()
	hostileLog := filepath.Join(hostileDir, "hostile.log")
	writeHostileMPMBin(t, hostileDir, hostileLog)
	t.Setenv("PATH", hostileDir)

	if _, err := runMpmCall(context.Background(), &lazyMPMBin{}, "mpm_provenance", []byte(`{}`)); err != nil {
		t.Fatalf("runMpmCall: %v", err)
	}

	if got := readCallLog(t, goodLog); !strings.Contains(got, good+" call mpm_provenance") {
		t.Errorf("call log = %q, want the pinned binary %q", got, good)
	}
	if got := readCallLog(t, hostileLog); got != "" {
		t.Errorf("hostile PATH mpm was executed: %q", got)
	}
}

// TestObserve_InvalidPathFailsClearly covers requirement N.
func TestObserve_InvalidPathFailsClearly(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-installed-mpm")

	_, err := runMpmCall(context.Background(), &lazyMPMBin{explicit: missing}, "mpm_provenance", []byte(`{}`))
	if err == nil {
		t.Fatal("runMpmCall succeeded with a nonexistent --mpm path")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name the invalid path %q", err, missing)
	}
	if !strings.Contains(err.Error(), "mpm_provenance") {
		t.Errorf("error = %v, want it to name the tool that needed the binary", err)
	}
}

// TestObserve_NoResolvableBinaryFailsRatherThanUsingPath is the anti-vacuity
// guard for the whole contract.
func TestObserve_NoResolvableBinaryFailsRatherThanUsingPath(t *testing.T) {
	hostileDir := t.TempDir()
	hostileLog := filepath.Join(hostileDir, "hostile.log")
	writeHostileMPMBin(t, hostileDir, hostileLog)
	t.Setenv("PATH", hostileDir)
	t.Setenv("MPM_BIN", "")

	_, err := runMpmCall(context.Background(), &lazyMPMBin{}, "mpm_provenance", []byte(`{}`))
	if err == nil {
		t.Fatal("runMpmCall succeeded with no configured binary; a PATH fallback still exists")
	}
	if got := readCallLog(t, hostileLog); got != "" {
		t.Errorf("hostile PATH mpm was used as a fallback: %q", got)
	}
}

// TestObserve_DryRunDoesNotRequireABinary pins the §9 dry-run requirement:
// resolution is deferred, so an observe run that never shells out must not
// fail merely because no mpm binary could be resolved.
func TestObserve_DryRunDoesNotRequireABinary(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_TELEMETRY_DB", filepath.Join(ws, "telemetry.db"))

	hostileDir := t.TempDir()
	hostileLog := filepath.Join(hostileDir, "hostile.log")
	writeHostileMPMBin(t, hostileDir, hostileLog)
	t.Setenv("PATH", hostileDir)
	t.Setenv("MPM_BIN", "")

	if err := runObserve([]string{"--dry-run", "--since", "1"}); err != nil {
		t.Fatalf("runObserve --dry-run = %v; dry-run must not require a resolvable mpm binary", err)
	}
	if got := readCallLog(t, hostileLog); got != "" {
		t.Errorf("dry-run executed the hostile PATH mpm: %q", got)
	}
}
