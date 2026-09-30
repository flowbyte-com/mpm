package main

// work_purge_cli_test.go — CLI-surface coverage for `mpm work item
// purge`.
//
// Design: docs/designs/2026-09-30-work-archive-and-purge.md §5.3, §5.7,
// §8.3, §8.4.
//
// Isolation: every test pins MPM_WORKSPACE to t.TempDir() and re-arms
// the process-wide getDB() singleton, so neither the CLI nor the
// assertions can reach the production MPM database (CLAUDE.md §3).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
	"github.com/stretchr/testify/require"
)

// purgeSeed creates a completed work item through the substrate API.
func purgeSeed(t *testing.T, dm *mpminternal.DatabaseManager, title string) string {
	t.Helper()
	w, err := dm.AddWork(title, "content", "")
	require.NoError(t, err)
	_, err = dm.CompleteWorkWithContext(w.ID, "done", mpminternal.ActiveContext{})
	require.NoError(t, err)
	return w.ID
}

// runPurge runs the real CLI entry point ONCE, capturing both the
// rendered stdout and the usererror stream, and returns the exit code
// plus the combined text.
//
// Three sinks, one invocation: captureBoth redirects os.Stdout and
// os.Stderr (the renderer writes to stdout, respond() to stderr), while
// usererror holds its own writer reference captured at package init —
// so redirecting os.Stderr does not reach it and SetWriter is the only
// way to see it.
func runPurge(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var buf purgeBuf
	usererror.SetWriter(&buf)
	defer usererror.SetWriter(nil)
	code := 0
	rendered := captureBoth(t, func() {
		code = runWorkItemArgsForTest(append([]string{"purge"}, args...))
	})
	return code, rendered + buf.String()
}

// purgeBuf is an io.Writer that accumulates usererror output.
type purgeBuf struct{ b strings.Builder }

func (p *purgeBuf) Write(b []byte) (int, error) { return p.b.Write(b) }
func (p *purgeBuf) String() string              { return p.b.String() }

// ─── Dry run is the default ───────────────────────────────────────

// TestPurgeCLI_DryRunIsDefault pins the single most important gate:
// invoking purge without --force must not write.
func TestPurgeCLI_DryRunIsDefault(t *testing.T) {
	dm := isolatedWorkDM(t)
	id := purgeSeed(t, dm, "cli dry run")

	code, out := runPurge(t, id, "--reason-code", "test_debris")
	require.Equal(t, 0, code, "a dry run is a successful report, not a failure: %s", out)

	// The report must say so plainly and name the re-run.
	require.Contains(t, out, "dry run", "output does not identify itself as a dry run")
	require.Contains(t, out, "nothing was written")
	require.Contains(t, out, "--force", "output does not tell the operator how to proceed")

	// Nothing was written.
	_, err := dm.GetWork(id)
	require.NoError(t, err, "the dry run removed the work item")
	require.Equal(t, 0, cliPurgeAuditCount(t, dm), "the dry run wrote an audit row")
}

// TestPurgeCLI_DryRunDisclosesResidualExposure pins the §5.7 mandatory
// disclosure. An operator who believes purge erased something is worse
// off than one who was never offered the command.
func TestPurgeCLI_DryRunDisclosesResidualExposure(t *testing.T) {
	dm := isolatedWorkDM(t)
	id := purgeSeed(t, dm, "cli residual exposure")

	_, out := runPurge(t, id, "--reason-code", "test_debris")
	for _, want := range []string{
		"NOT erasure",
		"no secure-erasure capability",
		"WAL",
		"backups/critic-pre/",
		"mpm backup",
	} {
		require.Contains(t, out, want,
			"the dry run does not disclose %q — an operator must not be able to read this as erasure", want)
	}
}

// TestPurgeCLI_DryRunReportsDeleteCounts pins that the dry run reports
// real numbers, not a table list.
func TestPurgeCLI_DryRunReportsDeleteCounts(t *testing.T) {
	dm := isolatedWorkDM(t)
	id := purgeSeed(t, dm, "cli counts")

	_, out := runPurge(t, id, "--reason-code", "test_debris")
	require.Contains(t, out, "work_events")
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM work_events WHERE work_id = ?`, id).Scan(&n))
	require.Greater(t, n, 0, "fixture should have a ledger")
	require.Regexp(t, `work_events[^\n]*\d+`, out,
		"the dry run should show a real count, and the fixture has %d events", n)
}

// ─── Reason code ──────────────────────────────────────────────────

// TestPurgeCLI_ReasonCodeRequired pins the required gate at the CLI.
func TestPurgeCLI_ReasonCodeRequired(t *testing.T) {
	dm := isolatedWorkDM(t)
	id := purgeSeed(t, dm, "cli no reason")

	code, out := runPurge(t, id)
	require.NotEqual(t, 0, code)
	require.Contains(t, out, "--reason-code")
	_, err := dm.GetWork(id)
	require.NoError(t, err, "a rejected purge still removed the work item")
}

// TestPurgeCLI_InvalidReasonCodeRejected pins that a bad code is a clear
// error naming the valid set, and that there is no free-text --reason.
func TestPurgeCLI_InvalidReasonCodeRejected(t *testing.T) {
	dm := isolatedWorkDM(t)
	id := purgeSeed(t, dm, "cli bad reason")

	for _, code := range []string{"privacy", "typo", "TEST_DEBRIS"} {
		t.Run(code, func(t *testing.T) {
			exit, out := runPurge(t, id, "--reason-code", code)
			require.NotEqual(t, 0, exit, "reason_code %q was accepted", code)
			require.Contains(t, out, "test_debris", "the error should name the valid codes")
		})
	}
}

// TestPurgeCLI_ReasonCodeRequiredEvenWithForce pins that --force does
// not bypass the reason code. A purge without a justification leaves an
// audit row nobody can interpret.
func TestPurgeCLI_ReasonCodeRequiredEvenWithForce(t *testing.T) {
	dm := isolatedWorkDM(t)
	id := purgeSeed(t, dm, "cli force without reason")

	code, _ := runPurge(t, id, "--force")
	require.NotEqual(t, 0, code)
	_, err := dm.GetWork(id)
	require.NoError(t, err)
}

// TestPurgeCLI_NoFreeTextReasonFlag pins §5.3: there is no `--reason`
// on this path. resolve_contradiction owns that name for a different,
// non-destructive meaning, and a second `--reason` with different
// semantics would be a trap.
func TestPurgeCLI_NoFreeTextReasonFlag(t *testing.T) {
	dm := isolatedWorkDM(t)
	id := purgeSeed(t, dm, "cli reason not reason-code")

	// --reason alone does not satisfy the requirement.
	exit, out := runPurge(t, id, "--reason", "because")
	require.NotEqual(t, 0, exit, "--reason was accepted as a justification: %s", out)
	require.Contains(t, out, "--reason-code")
}

// ─── Forced purge ─────────────────────────────────────────────────

// TestPurgeCLI_ForcedPurgeRemoves drives the full forced path and
// checks both the substrate and the audit record.
func TestPurgeCLI_ForcedPurgeRemoves(t *testing.T) {
	dm := isolatedWorkDM(t)
	id := purgeSeed(t, dm, "cli forced")

	code, out := runPurge(t, id, "--reason-code", "administrative", "--note", "housekeeping", "--force")
	require.Equal(t, 0, code, "forced purge failed: %s", out)
	require.Contains(t, out, "Work item purged")
	require.Contains(t, out, "work_purge_audit")

	_, err := dm.GetWork(id)
	require.Error(t, err, "the work item survived a forced purge")
	require.Equal(t, 0, cliPurgeEventsCount(t, dm, id))

	// The note permanently survives.
	var note string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COALESCE(note,'') FROM work_purge_audit WHERE work_id = ?`, id).Scan(&note))
	require.Equal(t, "housekeeping", note,
		"an operator note is independent input and must survive the purge")
}

// TestPurgeCLI_UnknownID pins that a mistyped id is a non-zero exit.
func TestPurgeCLI_UnknownID(t *testing.T) {
	isolatedWorkDM(t)
	code, _ := runPurge(t, "work-nope", "--reason-code", "other", "--force")
	require.NotEqual(t, 0, code)
}

// TestPurgeCLI_RequiresWorkID pins the missing-positional error.
func TestPurgeCLI_RequiresWorkID(t *testing.T) {
	isolatedWorkDM(t)
	code, out := runPurge(t, "--reason-code", "other")
	require.NotEqual(t, 0, code)
	require.Contains(t, out, "work_id")
}

// ─── Refusal ──────────────────────────────────────────────────────

// TestPurgeCLI_RefusalEnumeratesReferrers pins §6.2 at the CLI: a
// refusal must name the referrers, because resolving them is the next
// thing the operator does.
func TestPurgeCLI_RefusalEnumeratesReferrers(t *testing.T) {
	dm := isolatedWorkDM(t)
	id := purgeSeed(t, dm, "cli referenced")
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, dependencies, created_at, updated_at)
		VALUES (?, 'decisions', 'a decision', ?, 0, 0)`, mpminternal.GenerateID(), `["`+id+`"]`)
	require.NoError(t, err)

	code, out := runPurge(t, id, "--reason-code", "other", "--force")
	require.NotEqual(t, 0, code)
	require.Contains(t, out, "memory_dependency", "the refusal does not name the referring record: %s", out)
	require.Contains(t, out, "No changes were made")
	require.Contains(t, out, "--cascade", "the refusal should state that MPM has no cascade")

	_, err = dm.GetWork(id)
	require.NoError(t, err, "a refused purge removed the work item anyway")
}

// ─── §8.4 Absence of side effects ────────────────────────────────

// TestPurgeCLI_NoBackupOnDryRunOrForce pins that purge creates no
// backup on its own. --backup is explicit and opt-in.
func TestPurgeCLI_NoBackupOnDryRunOrForce(t *testing.T) {
	dm := isolatedWorkDM(t)

	// A dry run.
	id1 := purgeSeed(t, dm, "cli no backup dry run")
	_, _ = runPurge(t, id1, "--reason-code", "test_debris")
	// A forced purge.
	id2 := purgeSeed(t, dm, "cli no backup force")
	_, _ = runPurge(t, id2, "--reason-code", "test_debris", "--force")

	// Neither should have produced a dump anywhere in the workspace.
	ws := os.Getenv("MPM_WORKSPACE")
	require.NotEmpty(t, ws)
	var found []string
	require.NoError(t, filepath.Walk(ws, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(info.Name(), ".sql") || strings.Contains(info.Name(), "backup") {
			found = append(found, path)
		}
		return nil
	}))
	require.Empty(t, found, "purge created backup artifacts without --backup: %v", found)
}

// TestPurgeCLI_ExplicitBackupIsTakenBeforeDelete pins §5.8. The dump
// is opt-in and must exist when requested.
func TestPurgeCLI_ExplicitBackupIsTakenBeforeDelete(t *testing.T) {
	dm := isolatedWorkDM(t)
	id := purgeSeed(t, dm, "cli with backup")

	backup := filepath.Join(t.TempDir(), "pre-purge.sql")
	code, out := runPurge(t, id, "--reason-code", "administrative", "--backup", backup, "--force")
	require.Equal(t, 0, code, "forced purge with --backup failed: %s", out)

	info, err := os.Stat(backup)
	require.NoError(t, err, "--backup did not write a dump")
	require.Greater(t, info.Size(), int64(0), "--backup wrote an empty dump")

	body, err := os.ReadFile(backup)
	require.NoError(t, err)
	require.Contains(t, string(body), id,
		"the backup was taken AFTER the delete; --backup must precede the purge")
}

// TestPurgeCLI_BackupWarningNamesTheMaterial pins the §5.8 disclosure
// in the rendered output, not only in the help text.
func TestPurgeCLI_BackupWarningNamesTheMaterial(t *testing.T) {
	dm := isolatedWorkDM(t)
	id := purgeSeed(t, dm, "cli backup warning")
	backup := filepath.Join(t.TempDir(), "warn.sql")

	_, out := runPurge(t, id, "--reason-code", "administrative", "--backup", backup, "--force")
	require.Contains(t, out, backup)
	require.Contains(t, out, "contains the purged material",
		"the output must tell the operator their backup holds the purged content")
}

// TestPurgeCLI_NoBackupRequestedOnRefusedPurge pins §5.8 ordering: the
// dump is a full copy of the material being removed, so it is written
// only for a purge that will actually run. A refusal must not leave a
// copy of the content on disk as a side effect of a no-op command.
func TestPurgeCLI_NoBackupRequestedOnRefusedPurge(t *testing.T) {
	dm := isolatedWorkDM(t)
	id := purgeSeed(t, dm, "cli refused with backup")
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, dependencies, created_at, updated_at)
		VALUES (?, 'decisions', 'd', ?, 0, 0)`, mpminternal.GenerateID(), `["`+id+`"]`)
	require.NoError(t, err)

	backup := filepath.Join(t.TempDir(), "should-not-exist.sql")
	code, out := runPurge(t, id, "--reason-code", "other", "--backup", backup, "--force")
	require.NotEqual(t, 0, code)
	require.Contains(t, out, "No backup was written",
		"the operator must be told their requested dump does not exist")
	_, err = os.Stat(backup)
	require.True(t, os.IsNotExist(err),
		"a refused purge wrote a backup it never needed: %v", err)
}

// ─── Help parity ──────────────────────────────────────────────────

// TestPurgeHelp_AdvertisesSurfaceAndDisclosure pins §5.2/§5.3: the help
// must carry the enum, the dry-run default, the no-erasure statement,
// and the --backup warning. Help text is the only thing an operator
// reads before running an irreversible command.
func TestPurgeHelp_AdvertisesSurfaceAndDisclosure(t *testing.T) {
	isolatedWorkDM(t)
	out := captureStdout(t, printWorkItemHelp)
	for _, want := range []string{
		"purge <work_id>",
		"--reason-code",
		"--force",
		"--backup",
		"dry run",
		"NOT erasure",
		"NO secure-erasure capability",
		"backups/critic-pre/",
		"no --cascade",
		"survives the purge",
	} {
		require.Contains(t, out, want, "`mpm work item --help` does not mention %q", want)
	}
	// The enum is printed from the source of truth, not hardcoded.
	for _, code := range mpminternal.ValidWorkPurgeReasonCodes {
		require.Contains(t, out, code, "help does not list reason code %q", code)
	}
	// And the reason codes are joined with " | ", which is the
	// affordance the error messages use too.
	require.Contains(t, out, "test_debris | accidental | corrupted | migration_cleanup | administrative | other")
}

// TestPurgeHelp_UnknownSubcommandListsPurge pins the second, independent
// list that must not drift from the help.
func TestPurgeHelp_UnknownSubcommandListsPurge(t *testing.T) {
	isolatedWorkDM(t)
	var buf purgeBuf
	usererror.SetWriter(&buf)
	defer usererror.SetWriter(nil)
	require.NotEqual(t, 0, runWorkItemArgsForTest([]string{"definitely-not-a-subcommand"}))
	require.Contains(t, buf.String(), "purge",
		"the unknown-subcommand list omits purge: %q", buf.String())
}

// ─── helpers ──────────────────────────────────────────────────────

func cliPurgeAuditCount(t *testing.T, dm *mpminternal.DatabaseManager) int {
	t.Helper()
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM work_purge_audit`).Scan(&n))
	return n
}

func cliPurgeEventsCount(t *testing.T, dm *mpminternal.DatabaseManager, id string) int {
	t.Helper()
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM work_events WHERE work_id = ?`, id).Scan(&n))
	return n
}
