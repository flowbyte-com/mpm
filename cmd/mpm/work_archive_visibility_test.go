package main

// work_archive_visibility_test.go — CLI-side coverage for the work
// archive lifecycle and the visibility query axis.
//
// Design: docs/designs/2026-09-30-work-archive-and-purge.md §4.2, §9.
//
// The `mpm wake` completed_refs projection is exclusion site 7 of §3.1
// and the only one that lives in package main, so it is driven here
// through the real queryCompletedWorkRefs function rather than a copy
// of its SQL.
//
// Isolation: every test pins MPM_WORKSPACE to t.TempDir(), matching
// TestWorkItemCLI_GrammarContract. The production MPM database is never
// opened (CLAUDE.md §3, "Test isolation").

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
	"github.com/stretchr/testify/require"
)

// isolatedWorkDM pins the CLI's database to a throwaway workspace and
// returns the manager the CLI itself will use.
//
// Two resolution paths reach the substrate from handleWorkItem:
// `handleCall` opens a fresh DatabaseManager per invocation
// (openCallDM), while `handleWorkItemList` reads the process-wide
// getDB() singleton. The singleton resolves MPM_WORKSPACE once, on
// first use, and stays pinned for the rest of the package's run — so
// without re-arming it a test can seed one workspace and have the CLI
// read another. resetBackfillGlobalDB re-arms it, which is what makes
// t.Setenv actually isolate this test.
func isolatedWorkDM(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	resetBackfillGlobalDB(t)
	dm := getDBConcrete()
	require.NotNil(t, dm, "getDBConcrete returned nil")
	return dm
}

// finishAndArchive walks an item to the archived state through the
// substrate API (not the CLI), returning its id.
func finishAndArchive(t *testing.T, dm *mpminternal.DatabaseManager, title string) string {
	t.Helper()
	w, err := dm.AddWork(title, "", "")
	require.NoError(t, err)
	_, err = dm.CompleteWorkWithContext(w.ID, "done", mpminternal.ActiveContext{})
	require.NoError(t, err)
	_, _, err = dm.ArchiveWorkWithContext(w.ID, "", mpminternal.ActiveContext{})
	require.NoError(t, err)
	return w.ID
}

// ─── Site 7: `mpm wake` completed refs ────────────────────────────

// TestWakeCompletedRefs_ExcludesArchived pins exclusion site 7. The
// design's stated risk is a partial filter — an archived item leaking
// into exactly one surface — so every operational surface gets its own
// assertion rather than one integration pass.
func TestWakeCompletedRefs_ExcludesArchived(t *testing.T) {
	dm := newTestDMForCmd(t)

	archived := finishAndArchive(t, dm, "archived finished")
	visible, err := dm.AddWork("visible finished", "", "")
	require.NoError(t, err)
	_, err = dm.CompleteWorkWithContext(visible.ID, "done", mpminternal.ActiveContext{})
	require.NoError(t, err)

	refs := queryCompletedWorkRefs(dm)
	require.NotNil(t, refs, "must return a non-nil slice for a predictable JSON shape")
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, r.ID)
	}
	require.NotContains(t, ids, archived, "`mpm wake` completed_refs leaked archived work")
	require.Contains(t, ids, visible.ID, "`mpm wake` completed_refs dropped an active completed item")
}

// ─── CLI archive / unarchive ──────────────────────────────────────

// TestWorkItemCLI_ArchiveUnarchiveRoundTrip drives the full supported
// flow through the real CLI entry point and asserts the visibility
// filter actually changes what `mpm work item list` reports.
func TestWorkItemCLI_ArchiveUnarchiveRoundTrip(t *testing.T) {
	dm := isolatedWorkDM(t)

	w, err := dm.AddWork("finished item", "", "")
	require.NoError(t, err)
	_, err = dm.CompleteWorkWithContext(w.ID, "done", mpminternal.ActiveContext{})
	require.NoError(t, err)

	// Before archive: visible under the default filter.
	ids, env := listViaCLI(t, "--status", "all")
	require.True(t, containsID(ids, w.ID), "work should be in the default listing before archive; got %v", ids)
	require.Equal(t, "all", env["status"])
	require.Equal(t, "active", env["visibility"], "visibility must default to active")

	// Archive.
	require.Equal(t, 0, runWorkItemArgsForTest([]string{"archive", w.ID, "--note", "shipped"}))

	// Default filter hides it.
	ids, _ = listViaCLI(t, "--status", "all")
	require.False(t, containsID(ids, w.ID), "archived work must be excluded from the default listing; got %v", ids)

	// --visibility archived reveals it.
	ids, _ = listViaCLI(t, "--status", "all", "--visibility", "archived")
	require.True(t, containsID(ids, w.ID), "archived work must appear under --visibility archived; got %v", ids)

	// --visibility all is the union.
	ids, _ = listViaCLI(t, "--status", "all", "--visibility", "all")
	require.True(t, containsID(ids, w.ID), "archived work must appear under --visibility all; got %v", ids)

	// Unarchive restores it, still `done` — never reopened.
	require.Equal(t, 0, runWorkItemArgsForTest([]string{"unarchive", w.ID}))

	restored, err := dm.GetWork(w.ID)
	require.NoError(t, err)
	require.Equal(t, mpminternal.WorkStatusDone, restored.Status,
		"unarchive must never reopen work")

	ids, _ = listViaCLI(t, "--status", "all")
	require.True(t, containsID(ids, w.ID), "unarchived work must be back in the default listing; got %v", ids)

	ids, _ = listViaCLI(t, "--status", "all", "--visibility", "archived")
	require.False(t, containsID(ids, w.ID), "unarchived work must leave the archived filter; got %v", ids)
}

// TestWorkItemCLI_ArchiveRefusesOpen pins the terminal-only guard at
// the CLI boundary with a non-zero exit and no state change.
func TestWorkItemCLI_ArchiveRefusesOpen(t *testing.T) {
	dm := isolatedWorkDM(t)

	w, err := dm.AddWork("live commitment", "", "")
	require.NoError(t, err)

	require.NotEqual(t, 0, runWorkItemArgsForTest([]string{"archive", w.ID}),
		"archiving an open work item succeeded; want a non-zero exit")
	after, err := dm.GetWork(w.ID)
	require.NoError(t, err)
	require.False(t, after.IsArchived(), "refused archive must not set archived_at")

	ids, _ := listViaCLI(t, "--status", "all")
	require.True(t, containsID(ids, w.ID), "refused archive must leave the item in the default listing; got %v", ids)
}

// TestWorkItemCLI_ArchiveIdempotent pins §1.5 through the CLI: a second
// archive is not an error, and appends no second ledger event.
func TestWorkItemCLI_ArchiveIdempotent(t *testing.T) {
	dm := isolatedWorkDM(t)

	w, err := dm.AddWork("finished", "", "")
	require.NoError(t, err)
	_, err = dm.CompleteWorkWithContext(w.ID, "done", mpminternal.ActiveContext{})
	require.NoError(t, err)

	require.Equal(t, 0, runWorkItemArgsForTest([]string{"archive", w.ID}))
	require.Equal(t, 0, runWorkItemArgsForTest([]string{"archive", w.ID}),
		"a second archive must succeed idempotently")

	var n int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM work_events WHERE work_id = ? AND event_type = 'archived'`, w.ID).Scan(&n))
	require.Equal(t, 1, n, "a second archive must not append a second ledger event")
}

// TestWorkItemCLI_ArchiveUnknownID pins that a mistyped id is a
// non-zero exit, never a silent success.
func TestWorkItemCLI_ArchiveUnknownID(t *testing.T) {
	isolatedWorkDM(t)
	require.NotEqual(t, 0, runWorkItemArgsForTest([]string{"archive", "work-nope"}))
	require.NotEqual(t, 0, runWorkItemArgsForTest([]string{"unarchive", "work-nope"}))
}

// TestWorkItemCLI_VisibilityValidation pins the W-010 contract at the
// CLI boundary: a typo is a clear error, not a silent zero-row result.
func TestWorkItemCLI_VisibilityValidation(t *testing.T) {
	isolatedWorkDM(t)
	cases := []struct {
		name string
		args []string
	}{
		{"typo_rejected", []string{"list", "--visibility", "archivd"}},
		{"unknown_value_rejected_on_complete", []string{"list", "--visibility", "yes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NotEqual(t, 0, runWorkItemArgsForTest(tc.args),
				"invocation %v should have failed", tc.args)
		})
	}
}

// TestWorkItemCLI_ArchiveRequiresWorkID pins the missing-positional
// error path so a bare `mpm work item archive` fails loudly.
func TestWorkItemCLI_ArchiveRequiresWorkID(t *testing.T) {
	isolatedWorkDM(t)
	for _, sub := range []string{"archive", "unarchive"} {
		t.Run(sub, func(t *testing.T) {
			require.NotEqual(t, 0, runWorkItemArgsForTest([]string{sub}),
				"`mpm work item %s` with no id should have failed", sub)
		})
	}
}

// TestWorkItemCLI_AcceptsPointerForm pins that archive/unarchive accept
// the canonical `mpm://work/<id>` pointer form through the same
// NormalizeWorkID chokepoint as every other work subcommand.
func TestWorkItemCLI_AcceptsPointerForm(t *testing.T) {
	dm := isolatedWorkDM(t)

	w, err := dm.AddWork("finished", "", "")
	require.NoError(t, err)
	_, err = dm.CompleteWorkWithContext(w.ID, "done", mpminternal.ActiveContext{})
	require.NoError(t, err)

	require.Equal(t, 0, runWorkItemArgsForTest([]string{"archive", "mpm://work/" + w.ID}))
	after, err := dm.GetWork(w.ID)
	require.NoError(t, err)
	require.True(t, after.IsArchived(), "pointer form did not archive %s", w.ID)

	require.Equal(t, 0, runWorkItemArgsForTest([]string{"unarchive", "mpm://work/" + w.ID}))
	after, err = dm.GetWork(w.ID)
	require.NoError(t, err)
	require.False(t, after.IsArchived(), "pointer form did not unarchive %s", w.ID)
}

// ─── Help parity ──────────────────────────────────────────────────

// TestWorkItemHelp_AdvertisesArchiveSurface pins that the help text and
// the unknown-subcommand error list both name the new subcommands. The
// design (§4.4) requires all five help surfaces to change together, or
// agents receive a "valid subcommands include …" string that is a lie.
func TestWorkItemHelp_AdvertisesArchiveSurface(t *testing.T) {
	isolatedWorkDM(t)

	out := captureStdout(t, printWorkItemHelp)
	for _, want := range []string{
		"archive <work_id>",
		"unarchive <work_id>",
		"--visibility",
		"active|archived|all",
	} {
		require.Contains(t, out, want, "`mpm work item --help` does not mention %q", want)
	}

	// The unknown-subcommand message is a second, independent list that
	// must not drift from the help. It travels through usererror, which
	// captured os.Stderr at package init — so the pipe-redirect helpers
	// used elsewhere in this package do not see it. SetWriter is the
	// supported capture point.
	var buf bytes.Buffer
	usererror.SetWriter(&buf)
	defer usererror.SetWriter(nil)

	if code := runWorkItemArgsForTest([]string{"definitely-not-a-subcommand"}); code == 0 {
		t.Error("unknown subcommand exited 0")
	}
	combined := buf.String()
	for _, want := range []string{"archive", "unarchive"} {
		require.Contains(t, combined, want,
			"unknown-subcommand error does not list %q: %q", want, combined)
	}
}

// ─── helpers ──────────────────────────────────────────────────────

func containsID(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// listViaCLI runs `mpm work item list --json <args>` and returns the
// work ids plus the parsed envelope. Driving the real CLI path is what
// makes this a parity test rather than a second implementation.
func listViaCLI(t *testing.T, args ...string) ([]string, map[string]any) {
	t.Helper()
	full := append([]string{"list", "--json"}, args...)
	out := captureStdout(t, func() {
		if code := runWorkItemArgsForTest(full); code != 0 {
			t.Fatalf("`mpm work item %v` exited %d", full, code)
		}
	})
	// The envelope echoes both axes so an empty result is
	// distinguishable from a filtered one.
	var env map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &env),
		"list output was not a JSON envelope: %q", out)
	for _, key := range []string{"success", "works", "count", "status", "visibility"} {
		require.Contains(t, env, key, "list envelope missing %q: %s", key, out)
	}
	rows, _ := env["works"].([]any)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		row, _ := r.(map[string]any)
		id, _ := row["id"].(string)
		ids = append(ids, id)
	}
	return ids, env
}
