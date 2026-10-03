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
	"path/filepath"
	"strings"
	"sync"
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

// TestBlockedShred_NoFlagBypassesIt pins the three unavailable forms.
//
// These refuse UNCONDITIONALLY — the refusal is not behind the force
// check, because force cannot make them correct. This is the test that
// stops a future "cleanup" from quietly wiring -f through and enabling
// commands that either do not delete or destroy without rebuilding.
func TestBlockedShred_NoFlagBypassesIt(t *testing.T) {
	cases := []struct {
		target string
		// mustMention is a phrase the operator needs to act correctly.
		mustMention string
	}{
		{"sessions", "mpm memory shred"},
		{"memories", "mpm shred <id>"},
		{"database", "uninstall.sh"},
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
				if _, err := os.Stat(filepath.Join(ws, "src", "db", "mpm.db")); err == nil {
					t.Errorf("blocked `shred database` created or left a database behind")
				}
			})
		}
	}
}

// TestShredDatabase_NeverRemovesTheDatabaseFile is the specific guard for
// the highest-severity finding.
//
// The old implementation deleted mpm.db and then failed to rebuild it,
// because internal.NewMemoryStore never opens a database. If that code
// is ever restored behind a working flag path, this test fails.
func TestShredDatabase_NeverRemovesTheDatabaseFile(t *testing.T) {
	clearForce(t)
	ws := workspace(t)

	// Build a real database with real content.
	if code, out := runCLI(t, "kb", "memory", "add", "sentinel database content"); code != 0 {
		t.Fatalf("seed failed: exit %d\n%s", code, out)
	}
	dbPath := filepath.Join(ws, "src", "db", "mpm.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("could not build a database to protect: %v", err)
	}
	before, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	for _, argv := range [][]string{
		{"shred", "database"},
		{"shred", "database", "-f"},
		{"shred", "database", "--force"},
		{"shred", "-f", "database"},
	} {
		code, _ := runCLI(t, argv...)
		if code == 0 {
			t.Errorf("`mpm %s` succeeded; it must not be reachable", strings.Join(argv, " "))
		}
	}

	after, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("DATABASE FILE IS GONE after blocked shred database: %v", err)
	}
	if after.Size() != before.Size() {
		t.Errorf("database size changed %d -> %d across blocked invocations",
			before.Size(), after.Size())
	}
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
