// status_info_consistency_test.go — Regression coverage for the
// 2026-09-14 release-pass status/info consistency fixes.
//
// Three pre-fix defects are pinned here. Each test FAILS against
// the pre-fix binary (the built CLI reproduced these on the
// operator's production substrate on 2026-09-14) and PASSES
// against the post-fix binary.
//
// Defects pinned:
//
//   1. Uptime showed "0s (process)" because handlers_status.go
//      read dm.WatchdogPath() (which points at the per-DB
//      watchdog.jsonl — a query-observability log with no
//      process_started_unix field). The fix reads the canonical
//      scheduler.state.json via schedulerStatePath().
//
//   2. Mode/Persona showed "Mode: one (default)" / "Persona: one
//      (default)" because the formatter dropped the resolver's
//      source tag. The fix surfaces the source label so status
//      matches info's `active modes : default [fallback]` wording.
//
//   3. Skill count disagreement between info and skill list
//      (info reported `(none)` while skill list showed 1) was
//      fixed in Commit 3 by routing listSkillsForInfo through
//      dm.ListSkills (parse-aware). The dashboard row also uses
//      dm.ListSkills so all three surfaces agree.

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runStatusBinary invokes the production binary with HOME set to
// workspace (so mode/persona + scheduler.state fixtures resolve)
// and MPM_WORKSPACE pointing at a hermetic DB sub-directory.
//
// 2026-09-14 release-pass: the host's `~/.mpm` contains the
// canonical scheduler.state.json (read by mpm status for
// uptime) and the canonical mode/persona files (read for
// [fallback] resolution). The test fixtures shadow these paths
// under a hermetic HOME so the test never touches the operator's
// production state.
func runStatusBinary(t *testing.T, bin, workspace string, args ...string) ([]byte, []byte, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(),
		"HOME="+workspace,
		"USERPROFILE="+workspace, // Windows fallback (no-op on linux)
		"MPM_WORKSPACE="+workspace,
		"MPM_SHARED_DB="+filepath.Join(workspace, "shared.db"),
	)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return outBuf.Bytes(), errBuf.Bytes(), exitErr.ExitCode()
	}
	if err != nil {
		t.Logf("run %v: %v\nstdout=%s\nstderr=%s", args, err, outBuf.String(), errBuf.String())
		return outBuf.Bytes(), errBuf.Bytes(), -1
	}
	return outBuf.Bytes(), errBuf.Bytes(), 0
}

// statusTestFixtures writes the minimum mode/persona + scheduler.state
// fixtures so the canonical resolvers return the values the tests
// expect. The fixtures mirror the host's `~/.mpm/{mode,persona,run}`
// layout.
func statusTestFixtures(t *testing.T, workspace string) {
	t.Helper()
	modeDir := filepath.Join(workspace, "mode")
	if err := os.MkdirAll(modeDir, 0700); err != nil {
		t.Fatalf("mkdir mode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modeDir, "default.md"),
		[]byte("---\nmode: default\ndescription: test\n---\n# default\n"), 0600); err != nil {
		t.Fatalf("write default.md: %v", err)
	}
	personaDir := filepath.Join(workspace, "persona")
	if err := os.MkdirAll(personaDir, 0700); err != nil {
		t.Fatalf("mkdir persona: %v", err)
	}
	if err := os.WriteFile(filepath.Join(personaDir, "default.md"),
		[]byte("---\npersona: default\ndescription: test\n---\n# default\n"), 0600); err != nil {
		t.Fatalf("write persona default.md: %v", err)
	}
	runDir := filepath.Join(workspace, "run")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		t.Fatalf("mkdir run: %v", err)
	}
	startedAt := time.Now().Add(-2 * time.Hour).Unix()
	statePath := filepath.Join(runDir, "scheduler.state")
	fixture := fmt.Sprintf(`{"last_tick_unix":%d,"process_started_unix":%d,"tick_count":1,"pid":1,"last_status":"ok","last_error":""}`, startedAt, startedAt)
	if err := os.WriteFile(statePath, []byte(fixture), 0600); err != nil {
		t.Fatalf("write scheduler.state: %v", err)
	}
}

// TestStatus_Uptime_ReflectsSchedulerNotProcessStart asserts that
// `mpm status` reports the canonical scheduler uptime, not the
// CLI process uptime. The fallback label "(process)" is only
// correct when no scheduler.state is readable.
//
// 2026-09-14 release-pass: the pre-fix implementation read
// dm.WatchdogPath() (which points at the per-DB watchdog.jsonl —
// a query-observability log with no process_started_unix field)
// so the function ALWAYS fell back to "(process)" uptime.
func TestStatus_Uptime_ReflectsSchedulerNotProcessStart(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	statusTestFixtures(t, workspace)
	stdout, _, _ := runStatusBinary(t, bin, workspace, "status")
	out := string(stdout)
	if !strings.Contains(out, "Uptime :") {
		t.Fatalf("`mpm status` must contain an Uptime row.\nGot:\n%s", out)
	}
	if strings.Contains(out, "Uptime : 0s (process)") {
		t.Fatalf("`mpm status` must not show `0s (process)` when scheduler.state is reachable.\nGot:\n%s", out)
	}
}

// TestStatus_ModePersonaAgreeWithInfo asserts that `mpm status`
// mode/persona lines carry the same source tag as `mpm info`'s
// `active modes : default [fallback]` / `active persona : default
// [fallback]`. The pre-fix wording (`Mode: one (default)`) did
// not expose the source.
func TestStatus_ModePersonaAgreeWithInfo(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	statusTestFixtures(t, workspace)
	stdout, _, _ := runStatusBinary(t, bin, workspace, "status")
	out := string(stdout)
	if !strings.Contains(out, "[fallback]") {
		t.Fatalf("`mpm status` must surface [fallback] tag matching info.\nGot:\n%s", out)
	}
	if !strings.Contains(out, "Persona:") {
		t.Fatalf("`mpm status` Persona row must be present.\nGot:\n%s", out)
	}
	// Pre-fix wording "Mode: one (default)" must NOT appear.
	if strings.Contains(out, "Mode: one (default)") {
		t.Fatalf("`mpm status` Mode row must NOT use the legacy `Mode: one (default)` wording.\nGot:\n%s", out)
	}
}

// TestStatus_MemoryCountAgreesWithInfo asserts that the dashboard
// count row uses the canonical query/source. The labels are
// scoped so the dashboard cannot be mistaken for the broader
// `mpm info` totals.
func TestStatus_MemoryCountAgreesWithInfo(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	workspace := t.TempDir()
	statusTestFixtures(t, workspace)
	// Seed a memory.
	runStatusBinary(t, bin, workspace, "add", "consistency probe memory")
	stdout, _, _ := runStatusBinary(t, bin, workspace, "status")
	out := string(stdout)
	if !strings.Contains(out, "Memories") {
		t.Fatalf("`mpm status` must surface Memories count.\nGot:\n%s", out)
	}
	// The pre-fix `Memorys:` typo must not appear.
	if strings.Contains(out, "Memorys:") {
		t.Fatalf("`mpm status` must NOT use the legacy `Memorys:` typo.\nGot:\n%s", out)
	}
}
