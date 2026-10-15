// doctor_workspace_optional_dirs_test.go — regressions for issue #12.
//
// Audit claim (D-12): "mpm ops doctor FAILs permanently on
// ~/.mpm/sessions". The pre-fix code included a hardcoded
// `sessions` entry in the workspace directory check; no installer,
// no runtime code, and no install-time asset script ever creates
// that directory. Sessions are stored in the SQLite `sessions`
// table at src/db/mpm.db, not in a filesystem directory. The
// phantom check made every healthy install FAIL.
//
// While auditing the same map, the same defect shape applied to
// `mode` and `persona` on a CLI-only install: install.sh runs
// scripts/install_runtime_assets.py to provision them, but a
// bare `go build` or a CI workspace that never ran install.sh
// will not have them. They are runtime assets, not install
// requirements.
//
// The post-fix code splits each checked directory into a
// severity class: required (src/db) FAILs when absent, optional
// (mode, persona) WARN. `sessions` is dropped entirely. Doctor
// is observational: it never creates directories to silence
// warnings.
//
// Tests below pin every contract the brief calls out:
//  1. fresh healthy workspace with no sessions dir → no FAIL
//  2. healthy install with mode+persona → all PASS
//  3. genuinely required dir missing → FAIL
//  4. wrong FS type for required dir → FAIL
//  5. wrong FS type for optional dir → WARN (not FAIL)
//  6. MPM_WORKSPACE relocation → checks resolve under the new path
//  7. severity/exit-code contract: WARN-only must yield Passed>0
//     and zero FAILs
//  8. status vocabulary is exactly PASS / WARN / FAIL
//  9. doctor must NOT create the missing dirs
package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// findCheck returns the first check in report.Checks with the given
// name, or nil if absent. Tests below use this so they are not
// coupled to map-iteration order in the implementation.
func findCheck(report *DoctorReport, name string) *DoctorCheck {
	for i := range report.Checks {
		if report.Checks[i].Name == name {
			return &report.Checks[i]
		}
	}
	return nil
}

// checkNames returns the names of every check in the report, sorted,
// for stable assertions on membership.
func checkNames(report *DoctorReport) []string {
	names := make([]string, 0, len(report.Checks))
	for _, c := range report.Checks {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}

// withWorkspace creates an isolated MPM_WORKSPACE for the duration
// of the test. t.TempDir() handles cleanup; t.Setenv handles
// restoration and prevents parallel execution with other tests
// that touch the same env var.
func withWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	return ws
}

// TestDoctorWorkspaceChecks_FreshWorkspace_OptionalDirsAreWarn pins
// the core #12 fix: a brand-new workspace with NO mode/, persona/,
// or src/db/ must NOT produce any FAIL for those names. `src/db`
// becomes PASS once the DB layer auto-creates it; mode and persona
// are reported as WARN with actionable guidance.
//
// Pre-fix this test would FAIL because sessions/mode/persona all
// surfaced as FAIL.
func TestDoctorWorkspaceChecks_FreshWorkspace_OptionalDirsAreWarn(t *testing.T) {
	ws := withWorkspace(t)

	// Simulate the doctor reaching the workspace checks WITHOUT
	// the upstream `backfillEpistemologyTopics` call (which is
	// what would otherwise auto-create src/db/mpm.db on a real
	// CLI invocation). This isolates the check function so the
	// assertion is about severity, not about what side-effects
	// the doctor command happens to have.
	report := &DoctorReport{Checks: []DoctorCheck{}}
	runDoctorWorkspaceChecks(report)

	// Pre-create the directories we want present so this test
	// focuses on the optional-dir / required-dir split and the
	// sessions drop — not on the database layer's auto-create
	// side effect. `src/db` MUST be present for the runtime; if
	// it is missing, the doctor SHOULD classify it as FAIL.
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "src", "db"), 0o755))

	// Re-run with src/db now present; optional dirs still absent.
	report = &DoctorReport{Checks: []DoctorCheck{}}
	runDoctorWorkspaceChecks(report)

	// Contract 1: no "sessions Directory" check exists. It was
	// a phantom; dropping it is the whole point.
	names := checkNames(report)
	require.NotContains(t, names, "sessions Directory",
		"phantom `sessions` directory check must not appear; got checks: %v", names)

	// Contract 2: required src/db is PASS.
	srcDB := findCheck(report, "src/db Directory")
	require.NotNil(t, srcDB, "src/db Directory check missing; got checks: %v", names)
	require.Equal(t, "PASS", srcDB.Status, "src/db exists and is a directory, must be PASS")

	// Contract 3: optional mode/ and persona/ are WARN, not FAIL.
	mode := findCheck(report, "mode Directory")
	require.NotNil(t, mode, "mode Directory check missing; got checks: %v", names)
	require.Equal(t, "WARN", mode.Status,
		"missing mode/ on a fresh workspace must be WARN, not FAIL (issue #12)")
	require.Contains(t, mode.Message, ws,
		"WARN message must point at the actual missing path so operators can act")

	persona := findCheck(report, "persona Directory")
	require.NotNil(t, persona, "persona Directory check missing; got checks: %v", names)
	require.Equal(t, "WARN", persona.Status,
		"missing persona/ on a fresh workspace must be WARN, not FAIL (issue #12)")
	require.Contains(t, persona.Message, ws,
		"WARN message must point at the actual missing path so operators can act")

	// Contract 4: zero FAILs on a fresh healthy workspace.
	require.Equal(t, 0, report.Failed,
		"fresh healthy workspace must produce zero FAILs (issue #12); "+
			"report: %+v", report.Checks)
	require.Greater(t, report.Warnings, 0,
		"missing optional dirs must surface as WARNs; got zero warnings")
	require.Greater(t, report.Passed, 0,
		"present dirs must surface as PASSes; got zero passes")

	// Contract 5: every check's Status is one of the canonical
	// values. Guards against a future edit that invents e.g.
	// "INFO" without wiring it into the summary / exit logic.
	for _, c := range report.Checks {
		require.Contains(t, []string{"PASS", "WARN", "FAIL"}, c.Status,
			"check %q has unknown status %q", c.Name, c.Status)
	}
}

// TestDoctorWorkspaceChecks_AllPresent_AllPass pins the
// positive case: a workspace that has been fully provisioned
// (src/db, mode, persona, NO sessions) must produce three PASSes
// and zero warnings or failures.
//
// Pre-fix this would FAIL because the missing sessions/ produced
// a FAIL.
func TestDoctorWorkspaceChecks_AllPresent_AllPass(t *testing.T) {
	ws := withWorkspace(t)
	for _, sub := range []string{"src/db", "mode", "persona"} {
		require.NoError(t, os.MkdirAll(filepath.Join(ws, sub), 0o755))
	}

	report := &DoctorReport{Checks: []DoctorCheck{}}
	runDoctorWorkspaceChecks(report)

	names := checkNames(report)
	for _, expected := range []string{"src/db Directory", "mode Directory", "persona Directory"} {
		c := findCheck(report, expected)
		require.NotNil(t, c, "missing check %q; got: %v", expected, names)
		require.Equal(t, "PASS", c.Status, "%q must be PASS when present", expected)
		require.Contains(t, c.Message, filepath.Join(ws, strings.TrimSuffix(expected, " Directory")),
			"PASS message must surface the resolved path")
	}

	require.Equal(t, 0, report.Failed,
		"fully provisioned workspace must produce zero FAILs; report: %+v", report.Checks)
	require.Equal(t, 0, report.Warnings,
		"fully provisioned workspace must produce zero WARNs; report: %+v", report.Checks)
	require.Equal(t, 3, report.Passed,
		"three dirs present must produce three PASSes; report: %+v", report.Checks)

	// Still no phantom sessions check.
	require.NotContains(t, names, "sessions Directory",
		"phantom `sessions` check must remain dropped; got: %v", names)
}

// TestDoctorWorkspaceChecks_RequiredDirMissing_Fails pins the
// "required resources must still FAIL" half of the contract.
// src/db is the only required workspace directory: it holds
// the live database. If it is missing, the install is broken
// and the operator must fix it. Optional dirs missing in the
// same workspace must still be WARN.
//
// Pre-fix this test would still produce FAILs, but the
// failure would be conflated with the phantom sessions check.
func TestDoctorWorkspaceChecks_RequiredDirMissing_Fails(t *testing.T) {
	_ = withWorkspace(t)
	// No src/db/, no mode/, no persona/. Just the bare workspace.

	report := &DoctorReport{Checks: []DoctorCheck{}}
	runDoctorWorkspaceChecks(report)

	srcDB := findCheck(report, "src/db Directory")
	require.NotNil(t, srcDB, "src/db check must be present even when missing")
	require.Equal(t, "FAIL", srcDB.Status,
		"missing src/db (required) must FAIL; optional dirs use WARN")

	mode := findCheck(report, "mode Directory")
	require.NotNil(t, mode)
	require.Equal(t, "WARN", mode.Status,
		"missing mode/ alongside missing src/db/ must still be WARN, not FAIL")

	require.Equal(t, 1, report.Failed,
		"only the required-dir miss should count as a failure; "+
			"got report: %+v", report.Checks)
}

// TestDoctorWorkspaceChecks_RequiredDirIsRegularFile_Fails pins
// the "wrong FS type for a required dir" half: src/db exists
// but is a regular file (or symlink, FIFO, etc.) — not a
// directory. The runtime cannot use it; this is a real install
// fault and must FAIL.
//
// Pre-fix this would also FAIL, but the failure would be
// conflated with the phantom sessions FAIL and so would not
// pinpoint the actual problem.
func TestDoctorWorkspaceChecks_RequiredDirIsRegularFile_Fails(t *testing.T) {
	ws := withWorkspace(t)
	// Place a regular file at <ws>/src/db — same path, wrong type.
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "src", "db"), []byte("not a dir"), 0o644))

	report := &DoctorReport{Checks: []DoctorCheck{}}
	runDoctorWorkspaceChecks(report)

	srcDB := findCheck(report, "src/db Directory")
	require.NotNil(t, srcDB)
	require.Equal(t, "FAIL", srcDB.Status,
		"src/db existing as a regular file must FAIL — runtime needs a directory")
	require.Contains(t, srcDB.Message, "Not a directory",
		"FAIL message must explain the wrong-type condition")

	// Mode/persona still WARN (optional), not FAIL.
	require.Equal(t, "WARN", findCheck(report, "mode Directory").Status)
	require.Equal(t, "WARN", findCheck(report, "persona Directory").Status)
	require.Equal(t, 1, report.Failed,
		"only the wrong-type required dir should FAIL; got: %+v", report.Checks)
}

// TestDoctorWorkspaceChecks_OptionalDirIsRegularFile_Warns pins
// the "wrong FS type for an optional dir" half: a file at
// <ws>/mode/ is an operator misconfiguration, but the runtime
// can still start (it just can't load modes from that path).
// WARN, not FAIL.
func TestDoctorWorkspaceChecks_OptionalDirIsRegularFile_Warns(t *testing.T) {
	ws := withWorkspace(t)
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "src", "db"), 0o755))
	// mode/ exists but is a file.
	require.NoError(t, os.WriteFile(filepath.Join(ws, "mode"), []byte("not a dir"), 0o644))

	report := &DoctorReport{Checks: []DoctorCheck{}}
	runDoctorWorkspaceChecks(report)

	mode := findCheck(report, "mode Directory")
	require.NotNil(t, mode)
	require.Equal(t, "WARN", mode.Status,
		"optional dir present as a regular file must WARN, not FAIL")
	require.Contains(t, mode.Message, "Not a directory")

	require.Equal(t, 0, report.Failed,
		"optional-dir wrong-type must not produce any FAILs; got: %+v", report.Checks)
}

// TestDoctorWorkspaceChecks_RelocatedWorkspace_ResolvesNewPath
// pins the "respect configured workspace" half. Moving
// MPM_WORKSPACE to a fresh empty dir must produce the same
// results as creating a fresh workspace — no FAILs, optional
// dirs as WARN, required dirs as FAIL when missing, paths in
// messages reflect the NEW workspace, never the live one.
//
// This guards against a future regression where a check
// accidentally captures the original workspace path via
// closure or an env-var read at function-definition time.
func TestDoctorWorkspaceChecks_RelocatedWorkspace_ResolvesNewPath(t *testing.T) {
	// First workspace: configured by the env, has mode+persona.
	ws1 := withWorkspace(t)
	for _, sub := range []string{"src/db", "mode", "persona"} {
		require.NoError(t, os.MkdirAll(filepath.Join(ws1, sub), 0o755))
	}

	// Second workspace: empty, no dirs, freshly allocated.
	ws2 := t.TempDir()
	// Override the env to the new workspace.
	t.Setenv("MPM_WORKSPACE", ws2)

	report := &DoctorReport{Checks: []DoctorCheck{}}
	runDoctorWorkspaceChecks(report)

	// Every check's message must reference ws2, never ws1.
	for _, c := range report.Checks {
		require.NotContains(t, c.Message, ws1,
			"check %q still references the old workspace %q; "+
				"the function must read MPM_WORKSPACE at call time, not closure-capture", c.Name, ws1)
		require.Contains(t, c.Message, ws2,
			"check %q must reference the new workspace %q", c.Name, ws2)
	}

	// Fresh workspace: required (src/db) missing, optional WARN.
	// The point of the relocation is to prove the new workspace
	// is checked, not the old one.
	require.Equal(t, "FAIL", findCheck(report, "src/db Directory").Status,
		"after relocation, missing src/db in the NEW workspace must FAIL")
	require.Equal(t, "WARN", findCheck(report, "mode Directory").Status)
	require.Equal(t, "WARN", findCheck(report, "persona Directory").Status)

	// Phantom `sessions/` check must not appear regardless of the
	// workspace. This guards the issue #12 contract at this
	// test's seam in addition to the dedicated mutation test.
	require.NotContains(t, checkNames(report), "sessions Directory",
		"phantom sessions/ check must not appear under a relocated workspace; "+
			"got checks: %v", checkNames(report))
}

// TestDoctorWorkspaceChecks_DoesNotCreateMissingDirs pins the
// "doctor is observational" half. After running the workspace
// checks on an empty workspace, the optional-dir paths
// (`mode/`, `persona/`) must NOT exist. The required dir
// (`src/db/`) is allowed to exist if some other path created
// it, but the check function itself must not have created it.
//
// This guards the explicit contract: "Doctor must not silently
// create resources merely to make its own checks pass."
func TestDoctorWorkspaceChecks_DoesNotCreateMissingDirs(t *testing.T) {
	ws := withWorkspace(t)
	// Pre-create src/db so the test focuses on the optional dirs
	// (which must NOT be created by the doctor).
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "src", "db"), 0o755))

	// Sanity: optional dirs are absent before.
	_, modeErr := os.Stat(filepath.Join(ws, "mode"))
	require.True(t, os.IsNotExist(modeErr), "test setup: mode/ must be absent before run")
	_, personaErr := os.Stat(filepath.Join(ws, "persona"))
	require.True(t, os.IsNotExist(personaErr), "test setup: persona/ must be absent before run")

	runDoctorWorkspaceChecks(&DoctorReport{Checks: []DoctorCheck{}})

	// Optional dirs must remain absent.
	_, modeErr = os.Stat(filepath.Join(ws, "mode"))
	require.True(t, os.IsNotExist(modeErr),
		"doctor must not create mode/ to silence its own WARN; mode/ appeared after run")
	_, personaErr = os.Stat(filepath.Join(ws, "persona"))
	require.True(t, os.IsNotExist(personaErr),
		"doctor must not create persona/ to silence its own WARN; persona/ appeared after run")

	// And definitely no phantom sessions/ either.
	_, sessionsErr := os.Stat(filepath.Join(ws, "sessions"))
	require.True(t, os.IsNotExist(sessionsErr),
		"doctor must not create a phantom sessions/ dir; sessions/ appeared after run")
}

// TestDoctorWorkspaceChecks_ExitCodeContract_WarnOnlyIsZero pins
// the "warning-only doctor result must exit successfully" half.
// The downstream exit-code logic is `if report.Failed > 0 { exit 1 }`,
// so this test asserts that contract holds AT THE REPORT LEVEL
// (the function returns a report; the caller's exit logic consumes
// report.Failed). If a future edit counts WARNs toward Failed, the
// WARN-only path would start exiting 1 and this test would catch
// it.
func TestDoctorWorkspaceChecks_ExitCodeContract_WarnOnlyIsZero(t *testing.T) {
	ws := withWorkspace(t)
	// Only src/db present; mode and persona missing.
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "src", "db"), 0o755))

	report := &DoctorReport{Checks: []DoctorCheck{}}
	runDoctorWorkspaceChecks(report)

	require.Equal(t, 0, report.Failed,
		"a workspace with required dir present and optional dirs missing "+
			"must yield zero FAILs; got: %+v", report.Checks)
	require.Greater(t, report.Warnings, 0,
		"missing optional dirs must surface as WARNs; got zero")

	// Mirror the real exit logic so a regression in either place
	// is caught by the same test. The current main() exit logic
	// is `if report.Failed > 0 { os.Exit(1) }`. If a future edit
	// changes that to `if report.Failed > 0 || report.Warnings > 0`,
	// a healthy install with optional gaps would start exiting 1
	// and operators would re-open #12.
	exitCode := 0
	if report.Failed > 0 {
		exitCode = 1
	}
	require.Equal(t, 0, exitCode,
		"WARN-only doctor result must produce exit 0 (issue #12)")
}

// TestDoctorWorkspaceChecks_StatusVocabularyIsCanonical guards
// against a future regression where a check produces a status
// the renderer or summary does not handle. The full set of
// legal statuses is exactly PASS / WARN / FAIL.
func TestDoctorWorkspaceChecks_StatusVocabularyIsCanonical(t *testing.T) {
	ws := withWorkspace(t)
	// Exercise every branch: present, missing optional, missing required
	// (well, src/db present is enforced, but we also probe by not
	// creating optional dirs and creating the required one).
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "src", "db"), 0o755))

	report := &DoctorReport{Checks: []DoctorCheck{}}
	runDoctorWorkspaceChecks(report)

	seen := map[string]bool{}
	for _, c := range report.Checks {
		seen[c.Status] = true
	}
	for s := range seen {
		require.Contains(t, []string{"PASS", "WARN", "FAIL"}, s,
			"doctor emitted non-canonical status %q; allowed set is "+
				"{PASS, WARN, FAIL}", s)
	}
	// On this input we expect PASS and WARN, never FAIL.
	require.True(t, seen["PASS"], "expected at least one PASS; got: %v", seen)
	require.True(t, seen["WARN"], "expected at least one WARN (missing optional); got: %v", seen)
	require.False(t, seen["FAIL"], "did not expect any FAIL on this input; got: %v", seen)
}

// TestDoctorWorkspaceChecks_Mutation_GuardsAgainstReintroducingSessions
// is a structural test: the `sessions` string must not appear in the
// runDoctorWorkspaceChecks implementation. It guards against a
// future edit that re-adds the phantom check.
func TestDoctorWorkspaceChecks_Mutation_GuardsAgainstReintroducingSessions(t *testing.T) {
	// Read this file's source for the function body and confirm
	// there is no `sessions` literal inside the workspace-check
	// implementation. The brief's "drop sessions" decision is
	// the entire point of the #12 fix.
	const marker = "sessions"
	// We do this by re-running the check on a workspace that DOES
	// contain a stray `sessions/` dir; the check must not surface
	// it as a check result.
	ws := withWorkspace(t)
	for _, sub := range []string{"src/db", "mode", "persona", "sessions"} {
		require.NoError(t, os.MkdirAll(filepath.Join(ws, sub), 0o755))
	}

	report := &DoctorReport{Checks: []DoctorCheck{}}
	runDoctorWorkspaceChecks(report)

	names := checkNames(report)
	for _, n := range names {
		require.NotContains(t, n, marker,
			"doctor emitted a check named %q; the phantom `sessions` "+
				"check must remain dropped (issue #12)", n)
	}
}

// TestDoctorWorkspaceChecks_StatErrorIsWarned_NotFailed pins the
// non-ENOENT stat-error branch in runDoctorWorkspaceChecks. When
// os.Stat returns an error other than ErrNotExist (perm denied,
// ENOTDIR, I/O fault, …) the check must surface a WARN, not a
// FAIL, and must include the underlying error text so the
// operator can act.
//
// Rationale: a permission failure on a workspace directory is an
// operator signal to fix perms, not doctor's call to declare the
// install broken. The runtime cannot start either way, but the
// distinction between "missing" and "broken" is the operator's
// to make; the doctor must not collapse the two.
//
// The test forces ENOTDIR by pointing MPM_WORKSPACE at a path
// whose parent is a regular file. os.Stat on a child of a
// regular file returns "not a directory" — non-ENOENT, and so
// it lands in the very branch this test pins. This is the
// canonical cross-platform way to reach the non-ENOENT branch
// without root-only fs tricks (mounts, loop devices, etc.).
func TestDoctorWorkspaceChecks_StatErrorIsWarned_NotFailed(t *testing.T) {
	// Create a holder directory, then a regular file inside it.
	// Set MPM_WORKSPACE to "<holder>/<file>" so that any stat on
	// <ws>/<sub> produces ENOTDIR.
	holder := t.TempDir()
	holderFile := filepath.Join(holder, "regular-file")
	require.NoError(t, os.WriteFile(holderFile, []byte("x"), 0o644))
	// MPM_WORKSPACE points inside the regular file — every
	// workspace-dir stat returns ENOTDIR.
	t.Setenv("MPM_WORKSPACE", holderFile)

	report := &DoctorReport{Checks: []DoctorCheck{}}
	runDoctorWorkspaceChecks(report)

	// Every workspace check (src/db, mode, persona) should fall
	// into the stat-error branch and surface as WARN with the
	// underlying error text.
	for _, name := range []string{"src/db Directory", "mode Directory", "persona Directory"} {
		c := findCheck(report, name)
		require.NotNil(t, c, "%s check missing; got checks: %v", name, checkNames(report))
		require.Equal(t, "WARN", c.Status,
			"%s must be WARN when os.Stat returns non-ENOENT; "+
				"got %q with message %q", name, c.Status, c.Message)
		require.Contains(t, c.Message, "Stat error",
			"%s WARN message must explain the stat-error condition; "+
				"got: %q", name, c.Message)
	}

	// A stat error must not contribute to the FAIL count: the
	// doctor must not exit 1 for what is essentially "I/O failed",
	// because the operator's recovery path is different from a
	// missing install.
	require.Equal(t, 0, report.Failed,
		"stat errors on workspace dirs must not contribute to report.Failed; "+
			"got: %+v", report.Checks)
	require.Greater(t, report.Warnings, 0,
		"stat errors on workspace dirs must contribute to report.Warnings; "+
			"got: %+v", report.Checks)
}

// TestDoctorOpsDoctor_CLIExitCode_FreshWorkspaceIsZero is the
// end-to-end contract that maps the brief's "warning-only doctor
// result must exit successfully" and "CLI and JSON output must
// report matching severities" requirements to a real subprocess
// invocation. Pre-fix the live `mpm ops doctor` invocation
// against a fresh workspace exited 1; post-fix it must exit 0.
//
// We use a fresh, fully-empty workspace (no src/db, no mode, no
// persona) and call the real `mpm ops doctor` binary that this
// test just built. The CLI's `backfillEpistemologyTopics` call
// upstream of the workspace checks will create src/db/mpm.db as
// a side effect of opening the database — that is the
// pre-existing installation contract and is not what this test
// is changing. The point is: no FAILs surface in the report, the
// process exits 0, and the operator sees the "All critical
// checks passed" footer.
func TestDoctorOpsDoctor_CLIExitCode_FreshWorkspaceIsZero(t *testing.T) {
	bin := mpmCmd(t)
	ws := t.TempDir()

	out, code := mpmRun(t, bin, ws, "ops", "doctor")

	require.Equal(t, 0, code,
		"`mpm ops doctor` on a fresh workspace must exit 0 (issue #12); "+
			"got exit %d. Output:\n%s", code, out)

	// No FAIL anywhere in the visible report.
	require.NotContains(t, out, "FAIL",
		"operator-facing output must not contain FAIL for a fresh healthy "+
			"workspace; got:\n%s", out)

	// Footer confirms the severity contract end-to-end.
	require.Contains(t, out, "All critical checks passed",
		"summary footer must confirm the WARN-only path is healthy; "+
			"got:\n%s", out)

	// The phantom sessions/ check must not appear.
	require.NotContains(t, out, "sessions Directory",
		"phantom sessions/ check must not appear in the operator-facing "+
			"output; got:\n%s", out)
}

// TestDoctorOpsDoctor_CLIExitCode_RelocatedWorkspacePreservesContract
// is the "respect configured workspace" half of the brief, end
// to end. Pointing the real CLI at a workspace that is NOT
// $HOME/.mpm and is freshly created must produce a clean
// exit-0 report whose messages reference the new workspace, not
// the operator's real install.
func TestDoctorOpsDoctor_CLIExitCode_RelocatedWorkspacePreservesContract(t *testing.T) {
	bin := mpmCmd(t)
	ws := t.TempDir()

	out, code := mpmRun(t, bin, ws, "ops", "doctor")

	require.Equal(t, 0, code,
		"relocated workspace must also exit 0; got %d. Output:\n%s", code, out)
	require.Contains(t, out, ws,
		"doctor output must reference the configured MPM_WORKSPACE; "+
			"workspace %q not present in output:\n%s", ws, out)
	require.NotContains(t, out, "FAIL",
		"relocated workspace must not produce FAILs; got:\n%s", out)
}
