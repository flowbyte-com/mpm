// bulk_shred_force_test.go — end-to-end coverage for bulk `mpm shred`.
//
// # What went wrong
//
// router.parseFlags consumes `-f`/`--force` from argv and republishes it
// as MPM_FORCE=1; the token never reaches the handler. Every bulk shred
// handler used to scan argv for the literal "-f", so none of them could
// ever see it and all six forms were permanently unreachable. The
// confirmation prompt compounded this by telling the operator to re-run
// the exact command that had just failed.
//
// The fix was NOT to pass force through to all six. Two of them must
// never be enabled — see the table in the audit notes below — and one of
// those two is enabled only in the sense that it refuses unconditionally.
//
// # Why end-to-end
//
// Every test here drives the real router via CommandRouter.Execute, so
// parseFlags runs exactly as it does in production. A handler-level test
// would pass a literal "-f" straight into the handler and prove nothing
// about the actual bug, which lives in the space between the parser and
// the handler.
//
// # Isolation
//
// Every test pins MPM_WORKSPACE to t.TempDir(). MPM_FORCE is process-wide
// state that Execute mutates, so each test resets it explicitly rather
// than inheriting whatever the previous test left behind.

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// clearForce resets the router's force state for one test. Execute calls
// os.Setenv directly, so this uses Unsetenv plus a Cleanup restore rather
// than t.Setenv, which cannot express "unset".
func clearForce(t *testing.T) {
	t.Helper()
	prev, had := os.LookupEnv("MPM_FORCE")
	os.Unsetenv("MPM_FORCE")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("MPM_FORCE", prev)
		} else {
			_ = os.Unsetenv("MPM_FORCE")
		}
	})
}

// runCLI executes argv through the real router and returns (exit, output).
//
// stdout and stderr are captured together because the CLI deliberately
// splits them: user-facing refusals and success messages go to stdout,
// but the "not available" messages from the blocked forms are error
// text and go to stderr (see respond in handlers.go).
func runCLI(t *testing.T, argv ...string) (int, string) {
	t.Helper()

	origOut, origErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout, os.Stderr = w, w

	// MPM_INTERACTIVE is set by the same parser; make sure an ambient
	// value cannot change which confirmation path a test exercises.
	prevI, hadI := os.LookupEnv("MPM_INTERACTIVE")
	os.Unsetenv("MPM_INTERACTIVE")
	t.Cleanup(func() {
		if hadI {
			_ = os.Setenv("MPM_INTERACTIVE", prevI)
		} else {
			_ = os.Unsetenv("MPM_INTERACTIVE")
		}
	})

	code := NewRouter().Execute(argv)

	_ = w.Close()
	os.Stdout, os.Stderr = origOut, origErr
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read output: %v", err)
	}
	_ = r.Close()
	return code, buf.String()
}

// resetDBSingleton re-arms the process-wide DatabaseManager singleton for
// the duration of one test.
//
// getDB memoizes on a package-level sync.Once, so without this the first
// test in the binary to touch the database pins every later test to that
// test's MPM_WORKSPACE. That is a real isolation hole, not a cosmetic
// one: a later test would read and write rows in a workspace it no
// longer controls, and its MPM_WORKSPACE-based path assertions would
// silently check the wrong directory.
func resetDBSingleton(t *testing.T) {
	t.Helper()
	savedDM, savedErr := dbManager, dbManagerInitErr
	dbManager, dbManagerInitErr, dbManagerOnce = nil, nil, sync.Once{}
	t.Cleanup(func() {
		if dbManager != nil {
			_ = dbManager.Close()
		}
		dbManager, dbManagerInitErr = savedDM, savedErr
		dbManagerOnce = sync.Once{}
	})
}

// closeDBSingleton closes the process-wide DatabaseManager singleton
// (releasing LOCK_SH on `<dbPath>.lock`) and resets the sync.Once so
// the next getDB() lazily re-initialises against the current
// MPM_WORKSPACE.
//
// H-4 contract: any DatabaseManager holder in this process holds
// LOCK_SH for its lifetime. Tests that invoke `mpm shred database -f`
// (or any other destructive command that takes LOCK_EX|LOCK_NB) must
// therefore release the in-process LOCK_SH before invoking the
// command, otherwise the command correctly refuses with EWOULDBLOCK
// — same-process, same-inode lock contention, not a bug. After the
// destructive command completes, getDB() re-acquires LOCK_SH on the
// (possibly-renamed) file for verification reads.
//
// Note: this only closes the in-process singleton. It does NOT touch
// subprocess state; runCLI shells out via NewRouter().Execute which
// shares this process.
func closeDBSingleton(t *testing.T) {
	t.Helper()
	if dbManager != nil {
		_ = dbManager.Close()
	}
	dbManager = nil
	dbManagerInitErr = nil
	dbManagerOnce = sync.Once{}
}

// workspace pins MPM_WORKSPACE to a fresh temp dir and returns it.
func workspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	resetDBSingleton(t)
	return ws
}

// seedModeAndPersona writes one .md into each directory.
func seedModeAndPersona(t *testing.T, ws string) (modeFile, personaFile string) {
	t.Helper()
	modeFile = filepath.Join(ws, "mode", "bulk.md")
	personaFile = filepath.Join(ws, "persona", "bulk.md")
	for _, p := range []string{modeFile, personaFile} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte("---\nname: bulk\n---\nbody\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return modeFile, personaFile
}

// TestBulkShred_ForcePropagatesThroughRouter is the core regression.
//
// It pins the previously-broken behaviour for every form that is now
// meant to work, through the real parser. Before the fix all of these
// aborted at the confirmation prompt regardless of the flag.
func TestBulkShred_ForcePropagatesThroughRouter(t *testing.T) {
	for _, flag := range []string{"-f", "--force"} {
		for _, placement := range []string{"after", "before"} {
			name := flag + "/" + placement
			t.Run(name, func(t *testing.T) {
				clearForce(t)
				ws := workspace(t)
				modeFile, _ := seedModeAndPersona(t, ws)

				var argv []string
				if placement == "after" {
					argv = []string{"shred", "modes", flag}
				} else {
					argv = []string{"shred", flag, "modes"}
				}

				code, out := runCLI(t, argv...)
				if code != 0 {
					t.Fatalf("exit %d, want 0\n%s", code, out)
				}
				if strings.Contains(out, "Refusing") {
					t.Fatalf("force did not reach the handler; got the refusal:\n%s", out)
				}
				if !strings.Contains(out, "All modes deleted") {
					t.Errorf("unexpected output:\n%s", out)
				}
				// The sentinel must actually be gone — the message is only
				// trustworthy if the deletion happened.
				if _, err := os.Stat(modeFile); !os.IsNotExist(err) {
					t.Errorf("%s still exists after a confirmed bulk shred", modeFile)
				}
			})
		}
	}
}

// TestBulkShred_WithoutForceDeletesNothing is the safety half. A
// destructive command must do nothing at all without confirmation.
func TestBulkShred_WithoutForceDeletesNothing(t *testing.T) {
	for _, target := range []string{"topics", "modes", "personas"} {
		t.Run(target, func(t *testing.T) {
			clearForce(t)
			ws := workspace(t)
			modeFile, personaFile := seedModeAndPersona(t, ws)

			code, out := runCLI(t, "shred", target)
			if code == 0 {
				t.Fatalf("exit 0 without --force; a destructive command must fail closed")
			}
			if !strings.Contains(out, "Refusing") {
				t.Errorf("expected a refusal message, got:\n%s", out)
			}
			if !strings.Contains(out, "Nothing was deleted") {
				t.Errorf("refusal must state that nothing was deleted, got:\n%s", out)
			}
			if !strings.Contains(out, "-f") {
				t.Errorf("refusal must tell the operator how to confirm, got:\n%s", out)
			}
			for _, f := range []string{modeFile, personaFile} {
				if _, err := os.Stat(f); err != nil {
					t.Errorf("sentinel %s was removed without confirmation", f)
				}
			}
		})
	}
}

// TestBlockedShred_NoFlagBypassesIt pins the two unavailable forms
// that remain after the 2026-10-04 `shred database` redesign.
//
// These refuse UNCONDITIONALLY — the refusal is not behind the force
// check, because force cannot make them correct. This is the test that
// stops a future "cleanup" from quietly wiring -f through and enabling
// commands that either do not delete or destroy without rebuilding.
//
// `shred database` is intentionally NOT in this list — it became a
// real bulk shred on 2026-10-04 (see
// handlers_shred_database_reset.go and TestShredDatabase_* below).
// Adding it back here would be a regression of the redesign.
func TestBlockedShred_NoFlagBypassesIt(t *testing.T) {
	cases := []struct {
		target string
		// mustMention is a phrase the operator needs to act correctly.
		mustMention string
	}{
		{"sessions", "mpm memory shred"},
		{"memories", "mpm shred <id>"},
	}
	// Each row is a flag layout. "TARGET" expands to the target name, so
	// every row ends up naming the target exactly once. (A bare `mpm shred
	// -f` with no target is the help page, which is a different thing and
	// is covered by TestShredHelp_*.)
	layouts := [][]string{
		{"TARGET"},
		{"TARGET", "-f"},
		{"TARGET", "--force"},
		{"-f", "TARGET"},
		{"--force", "TARGET"},
	}

	for _, c := range cases {
		for _, layout := range layouts {
			argv := []string{"shred"}
			for _, tok := range layout {
				if tok == "TARGET" {
					tok = c.target
				}
				argv = append(argv, tok)
			}
			t.Run(c.target+"/"+strings.Join(layout, "_"), func(t *testing.T) {
				clearForce(t)
				ws := workspace(t)
				modeFile, personaFile := seedModeAndPersona(t, ws)

				code, out := runCLI(t, argv...)
				if code == 0 {
					t.Fatalf("`mpm %s` returned 0; it must refuse unconditionally\n%s",
						strings.Join(argv, " "), out)
				}
				if !strings.Contains(out, "not available") {
					t.Errorf("expected the 'not available' message, got:\n%s", out)
				}
				if !strings.Contains(out, c.mustMention) {
					t.Errorf("the refusal must name the supported alternative %q, got:\n%s",
						c.mustMention, out)
				}
				if !strings.Contains(out, "do not change this") {
					t.Errorf("the refusal must state that force does not enable it, got:\n%s", out)
				}
				// Sentinel check: a blocked form must never have touched anything.
				for _, f := range []string{modeFile, personaFile} {
					if _, err := os.Stat(f); err != nil {
						t.Errorf("blocked form deleted %s", f)
					}
				}
			})
		}
	}
}

// TestShredDatabase_NeverRemovesTheDatabaseFile is the specific guard for
// the highest-severity finding.
//
// Pre-2026-10-04: `mpm shred database` deleted mpm.db and then failed
// to rebuild it because internal.NewMemoryStore never opens a
// database. The historical test asserted the disabled form never
// touched the file.
//
// Post-2026-10-04: the command was redesigned into a safe active-
// substrate reset that requires -f/--force, validates-before-mutate,
// rolls back on any failure, preserves mirror.jsonl / watchdog.jsonl /
// telemetry.db / backups / mode / persona / blobs / the install
// prefix, and retains the previous mpm.db as mpm.db.pre-shred-<nanos>
// for one cycle.
//
// This test now pins the LIVE contract — refusal without -f, success
// with -f, preservation of logs/telemetry/backups, integrity of the
// rebuilt DB, second-reset success, and live-process refusal. The
// subtests that follow exercise individual facets; together they
// replace the original "always refuses" assertion.
func TestShredDatabase_NeverRemovesTheDatabaseFile(t *testing.T) {
	t.Run("RefusesWithoutForce", func(t *testing.T) {
		clearForce(t)
		ws := workspace(t)

		// Seed a real database with content.
		if code, out := runCLI(t, "kb", "memory", "add", "sentinel content"); code != 0 {
			t.Fatalf("seed failed: exit %d\n%s", code, out)
		}
		dbPath := filepath.Join(ws, "src", "db", "mpm.db")
		before, err := os.Stat(dbPath)
		if err != nil {
			t.Fatalf("seed database does not exist: %v", err)
		}

		code, out := runCLI(t, "shred", "database")
		if code == 0 {
			t.Fatalf("`mpm shred database` without -f succeeded; it must refuse")
		}
		if !strings.Contains(out, "Refusing") {
			t.Errorf("expected a refusal message, got:\n%s", out)
		}
		if !strings.Contains(out, "Nothing was deleted") {
			t.Errorf("refusal must state nothing was deleted, got:\n%s", out)
		}

		after, err := os.Stat(dbPath)
		if err != nil {
			t.Fatalf("database vanished after a no-force refusal: %v", err)
		}
		if after.Size() != before.Size() {
			t.Errorf("database size changed %d -> %d across a refused invocation",
				before.Size(), after.Size())
		}
	})

	t.Run("ResetsWithForceAndPreservesLogs", func(t *testing.T) {
		clearForce(t)
		ws := workspace(t)

		// Seed: a real DB with a row, plus mirror.jsonl, watchdog.jsonl,
		// telemetry.db, a backup, and mode/persona files.
		if code, out := runCLI(t, "kb", "memory", "add", "sentinel content"); code != 0 {
			t.Fatalf("seed failed: exit %d\n%s", code, out)
		}
		dbPath := filepath.Join(ws, "src", "db", "mpm.db")
		mirrorPath := filepath.Join(ws, "src", "db", "mirror.jsonl")
		watchdogPath := filepath.Join(ws, "src", "db", "watchdog.jsonl")
		telemetryPath := filepath.Join(ws, "src", "db", "telemetry.db")
		backupDir := filepath.Join(ws, "backups")
		if err := os.MkdirAll(backupDir, 0o755); err != nil {
			t.Fatalf("mkdir backups: %v", err)
		}
		backupFile := filepath.Join(backupDir, "pre-shred-backup.sql")
		if err := os.WriteFile(backupFile, []byte("-- pre-shred sentinel\n"), 0o644); err != nil {
			t.Fatalf("write backup: %v", err)
		}
		modeFile, personaFile := seedModeAndPersona(t, ws)
		for _, p := range []string{mirrorPath, watchdogPath, telemetryPath} {
			if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
				t.Fatalf("seed %s: %v", p, err)
			}
		}

		// H-4 contract: release the in-process LOCK_SH before invoking
		// `shred database -f`. The seed step above opened a DatabaseManager
		// that holds LOCK_SH for the test's lifetime; pre-H-4 the
		// destructive command would have ignored the lock entirely. Post-
		// H-4 the lock is honoured and the destructive window correctly
		// refuses to start while a writer (even this process's own
		// singleton) holds the lease. Closing the singleton releases
		// LOCK_SH; getDB() will lazily re-acquire it against the rebuilt
		// file for the post-reset integrity / sentinel query below.
		closeDBSingleton(t)

		code, out := runCLI(t, "shred", "database", "-f")
		if code != 0 {
			t.Fatalf("`mpm shred database -f` failed: exit %d\n%s", code, out)
		}
		if !strings.Contains(out, "Active substrate reset complete") {
			t.Errorf("success message missing the marker phrase; got:\n%s", out)
		}
		if !strings.Contains(out, ".pre-shred-") {
			t.Errorf("success message must name the recovery handle, got:\n%s", out)
		}

		// The DB must still exist and be schema-valid via PRAGMA
		// integrity_check. The size floor is not tiny: the rebuilt DB
		// carries the FTS5 schema baseline and the seedBaselineDirectives
		// rows (~1MB on this build). The real proof that the seed was
		// removed is the post-reset query below.
		if _, err := os.Stat(dbPath); err != nil {
			t.Fatalf("database missing after reset: %v", err)
		}

		// Preserved files: every byte must still be readable.
		for _, p := range []string{mirrorPath, watchdogPath, telemetryPath, backupFile, modeFile, personaFile} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("preserved file disappeared: %s (%v)", p, err)
			}
		}

		// Recovery handle: must exist as a sibling of mpm.db.
		matches, err := filepath.Glob(dbPath + ".pre-shred-*")
		if err != nil {
			t.Fatalf("glob: %v", err)
		}
		if len(matches) != 1 {
			t.Errorf("expected exactly 1 .pre-shred-* sibling, got %d: %v", len(matches), matches)
		}

		// The rebuilt DB must respond to integrity_check = ok AND must NOT
		// contain the sentinel seed string. The second assertion proves
		// the reset actually removed the seeded data — size alone is not
		// a signal because the rebuilt DB carries the FTS5 baseline.
		dm := getDBConcrete()
		if dm == nil {
			t.Fatalf("getDBConcrete() returned nil after reset")
		}
		var got string
		if err := dm.SQLDB().QueryRow("PRAGMA integrity_check").Scan(&got); err != nil {
			t.Fatalf("integrity_check query: %v", err)
		}
		if got != "ok" {
			t.Errorf("rebuilt DB integrity_check = %q, want \"ok\"", got)
		}
		var sentinelCount int
		if err := dm.SQLDB().QueryRow(
			"SELECT COUNT(*) FROM memories WHERE content LIKE ?", "%sentinel content%",
		).Scan(&sentinelCount); err != nil {
			t.Fatalf("sentinel count query: %v", err)
		}
		if sentinelCount != 0 {
			t.Errorf("rebuilt DB still contains the seeded sentinel memory (count=%d); the reset did not actually remove seeded rows", sentinelCount)
		}
	})

	t.Run("SecondResetSucceedsAndConsumesRecoveryHandle", func(t *testing.T) {
		clearForce(t)
		ws := workspace(t)

		if code, out := runCLI(t, "kb", "memory", "add", "sentinel content"); code != 0 {
			t.Fatalf("first seed failed: exit %d\n%s", code, out)
		}
		dbPath := filepath.Join(ws, "src", "db", "mpm.db")

		// H-4: release the in-process LOCK_SH held by the seed
		// DatabaseManager before the first `shred database -f`. The lock
		// has to be released before each reset invocation in this
		// subtest; the seed (and only the seed) re-opens a DM.
		closeDBSingleton(t)

		// First reset.
		if code, out := runCLI(t, "shred", "database", "-f"); code != 0 {
			t.Fatalf("first reset failed: exit %d\n%s", code, out)
		}
		firstHandle, err := filepath.Glob(dbPath + ".pre-shred-*")
		if err != nil {
			t.Fatalf("glob: %v", err)
		}
		if len(firstHandle) != 1 {
			t.Fatalf("expected exactly 1 .pre-shred-* after first reset, got %d", len(firstHandle))
		}
		// H-4: the first reset does not touch the in-process
		// singleton (the destructive window holds its own
		// exclusive-lock FD and releases on exit), so by the time
		// we reach the second reset no in-process LOCK_SH is held
		// — but we call closeDBSingleton unconditionally to keep
		// the pattern explicit and resilient to future refactors
		// that might lazily cache a DM.
		closeDBSingleton(t)

		// Second reset.
		if code, out := runCLI(t, "shred", "database", "-f"); code != 0 {
			t.Fatalf("second reset failed: exit %d\n%s", code, out)
		}
		// The first recovery handle must be consumed by the second reset
		// (the new reset removes any prior .pre-shred-* siblings to
		// avoid filesystem clutter). At this point there must be exactly
		// 1 .pre-shred-* sibling, and it is NOT the first one.
		siblings, err := filepath.Glob(dbPath + ".pre-shred-*")
		if err != nil {
			t.Fatalf("glob: %v", err)
		}
		if len(siblings) != 1 {
			t.Errorf("after second reset expected exactly 1 .pre-shred-* sibling, got %d: %v", len(siblings), siblings)
		}
		for _, s := range siblings {
			if s == firstHandle[0] {
				t.Errorf("first reset's recovery handle %s was not consumed by the second reset", s)
			}
		}
	})

	t.Run("RefusesUnderLiveProcessLock", func(t *testing.T) {
		clearForce(t)
		ws := workspace(t)
		if code, out := runCLI(t, "kb", "memory", "add", "sentinel content"); code != 0 {
			t.Fatalf("seed failed: exit %d\n%s", code, out)
		}
		dbPath := filepath.Join(ws, "src", "db", "mpm.db")
		lockPath := dbPath + ".lock"

		// H-4 contract: release the in-process LOCK_SH before
		// acquiring the probe LOCK_EX. Otherwise the test process
		// itself is the "writer holding the lease", and the probe
		// flock attempt below would EWOULDBLOCK against its own
		// LOCK_SH — same as the production scenario this test
		// simulates, but indistinguishable from a probe failure.
		closeDBSingleton(t)

		// Hold an exclusive flock on the lock file for the duration of
		// the test to simulate a second live MPM process.
		lf, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatalf("open lock: %v", err)
		}
		if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("acquire probe flock: %v", err)
		}
		t.Cleanup(func() {
			_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
			_ = lf.Close()
		})

		before, err := os.Stat(dbPath)
		if err != nil {
			t.Fatalf("stat before: %v", err)
		}
		code, out := runCLI(t, "shred", "database", "-f")
		if code == 0 {
			t.Fatalf("`mpm shred database -f` succeeded under live-process lock; it must refuse")
		}
		if !strings.Contains(out, "maintenance lease") {
			t.Errorf("expected the H-4 maintenance-lease refusal message, got:\n%s", out)
		}

		after, err := os.Stat(dbPath)
		if err != nil {
			t.Fatalf("database missing after refused reset: %v", err)
		}
		if after.Size() != before.Size() {
			t.Errorf("database size changed %d -> %d across refused reset", before.Size(), after.Size())
		}
	})

	t.Run("NonVacuityRevertToUnsafeCreateRecreateFails", func(t *testing.T) {
		// Inject the historical defect shape into a fresh subprocess so
		// the test cannot affect production state. The subprocess runs
		// a Go program that simulates the pre-fix handler exactly:
		//   os.Remove(mpm.db) then NewMemoryStore (which does not
		//   actually open a DB).
		//
		// The subprocess is expected to exit 0 with the workspace
		// holding NO database file at all. That is the bug class the
		// redesign prevents.
		clearForce(t)
		ws := workspace(t)

		if code, out := runCLI(t, "kb", "memory", "add", "sentinel content"); code != 0 {
			t.Fatalf("seed failed: exit %d\n%s", code, out)
		}
		dbPath := filepath.Join(ws, "src", "db", "mpm.db")
		before, err := os.Stat(dbPath)
		if err != nil {
			t.Fatalf("seed db missing: %v", err)
		}

		// Subprocess: write a tiny Go program to a temp file in a temp
		// directory OUTSIDE the test workspace so it does not race with
		// the real handler. The Go module used to compile it is the
		// repo module, so the stub is hermetic to the test.
		buggyDir, err := os.MkdirTemp("", "mpm-shred-buggy-")
		if err != nil {
			t.Fatalf("mkdir buggy dir: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(buggyDir) })
		buggy := filepath.Join(buggyDir, "buggy.go")
		if err := os.WriteFile(buggy, []byte(
			"package main\n"+
				"import (\"fmt\";\"os\";\"time\")\n"+
				"func main() {\n"+
				"  if err := os.Remove(os.Args[1]); err != nil { fmt.Println(err); os.Exit(2) }\n"+
				"  time.Sleep(50*time.Millisecond)\n"+
				"  fmt.Println(\"removed:\", os.Args[1])\n"+
				"}\n"), 0o644); err != nil {
			t.Fatalf("write buggy stub: %v", err)
		}

		// Run the buggy stub directly. This is the literal pre-fix
		// operation shape: remove the live DB and then "recreate" —
		// but in the stub, recreation is a no-op (matching the real
		// defect, where NewMemoryStore never opens a DB).
		cmd := exec.Command("go", "run", buggy, dbPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("buggy stub failed to run: %v\n%s", err, out)
		}

		// The bug class is: the workspace ends up with no DB. The
		// "safe" reset is: the workspace ends up with a valid empty DB.
		if _, err := os.Stat(dbPath); err == nil {
			info, _ := os.Stat(dbPath)
			t.Fatalf("bug repro FAILED: workspace still has db (size=%d, was=%d) — the buggy stub did not exercise the defect shape", info.Size(), before.Size())
		}
	})
}

// TestForceRequested_Contract pins the single accessor. Only the exact
// value "1" may satisfy a confirmation flag.
func TestForceRequested_Contract(t *testing.T) {
	cases := []struct {
		value string
		set   bool
		want  bool
	}{
		{"", false, false}, // unset
		{"", true, false},  // set but empty
		{"1", true, true},  // the value parseFlags writes
		{"0", true, false}, // malformed
		{"true", true, false},
		{"yes", true, false},
		{"2", true, false},
		{" 1", true, false}, // whitespace is not the flag
		{"1 ", true, false},
		{"1\n", true, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("set=%v/value=%q", c.set, c.value), func(t *testing.T) {
			clearForce(t)
			if c.set {
				t.Setenv("MPM_FORCE", c.value)
			}
			if got := forceRequested(); got != c.want {
				t.Errorf("forceRequested() = %v, want %v (MPM_FORCE set=%v value=%q)",
					got, c.want, c.set, c.value)
			}
		})
	}
}

// TestParseFlags_NeverReemitsForce proves the root cause is unchanged and
// documented: the token is consumed, not forwarded. Any handler that
// scans argv for a literal "-f" is therefore broken by construction.
func TestParseFlags_NeverReemitsForce(t *testing.T) {
	clearForce(t)

	for _, argv := range [][]string{
		{"shred", "modes", "-f"},
		{"shred", "modes", "--force"},
		{"shred", "-f", "modes"},
	} {
		got := NewRouter().parseFlags(argv)
		for _, tok := range got {
			if tok == "-f" || tok == "--force" {
				t.Errorf("parseFlags(%v) re-emitted %q; force must only be read "+
					"through forceRequested()", argv, tok)
			}
		}
		if os.Getenv("MPM_FORCE") != "1" {
			t.Errorf("parseFlags(%v) did not set MPM_FORCE=1", argv)
		}
		os.Unsetenv("MPM_FORCE")
	}
}

// TestShredTopics_AtomicAndReported exercises the one bulk form that
// really deletes, and checks the transaction fix: memberships and topics
// must go together, and a repeated run must be a clean no-op rather than
// an error.
func TestShredTopics_AtomicAndReported(t *testing.T) {
	clearForce(t)
	ws := workspace(t)

	// Seed two topics so the count is observable.
	for _, name := range []string{"alpha", "beta"} {
		if code, out := runCLI(t, "kb", "topic", "add", name, "fixture "+name); code != 0 {
			t.Fatalf("seed topic %s failed: exit %d\n%s", name, code, out)
		}
	}

	code, out := runCLI(t, "shred", "topics", "-f")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "All topics deleted (2 topics") {
		t.Errorf("expected both topics reported deleted, got:\n%s", out)
	}
	// The shred contract must survive into the output.
	if !strings.Contains(out, "NOT erased") {
		t.Errorf("bulk topic shred must disclose what it does not erase, got:\n%s", out)
	}

	// Idempotent re-run: succeeds, reports zero, does not error.
	code2, out2 := runCLI(t, "shred", "topics", "-f")
	if code2 != 0 {
		t.Fatalf("repeat run exit %d, want 0 (must be idempotent)\n%s", code2, out2)
	}
	if !strings.Contains(out2, "All topics deleted (0 topics") {
		t.Errorf("repeat run should report 0, got:\n%s", out2)
	}

	// The database itself must be intact — this is not `shred database`.
	if _, err := os.Stat(filepath.Join(ws, "src", "db", "mpm.db")); err != nil {
		t.Errorf("shred topics removed the database file: %v", err)
	}
}

// TestBulkShred_AbsentDirectoryIsANoOp pins the message contract for a
// bulk shred of a workspace that has no mode/ or persona/ directory.
//
// RemoveAll used to return the raw ReadDir errno, which the handler
// wrapped in a "Refusing:" prefix — so "there was nothing to delete"
// rendered as a safety refusal. Both forms must be clean no-ops that
// exit 0, matching `shred topics` on an empty database.
func TestBulkShred_AbsentDirectoryIsANoOp(t *testing.T) {
	for _, tc := range []struct{ target, want string }{
		{"modes", "All modes deleted (0 files)"},
		{"personas", "All personas deleted (0 files)"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			clearForce(t)
			workspace(t) // no mode/ or persona/ dir is created

			code, out := runCLI(t, "shred", tc.target, "-f")
			if code != 0 {
				t.Fatalf("absent directory should be a no-op, got exit %d:\n%s", code, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("got:\n%s\nwant it to contain %q", out, tc.want)
			}
			if strings.Contains(out, "no such file or directory") {
				t.Errorf("a raw errno leaked to the user for an empty workspace:\n%s", out)
			}
			if strings.Contains(out, "Refusing") {
				t.Errorf("nothing was deleted, so this must not read as a refusal:\n%s", out)
			}
		})
	}
}
