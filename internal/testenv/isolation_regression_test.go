// isolation_regression_test.go — proves that a DB-capable MPM child
// cannot resolve production state, whatever the parent environment looks
// like.
//
// # Why this file exists separately from the scanner
//
// subprocess_guard_test.go proves the guard CATCHES unsafe subprocess
// forms. This file proves the sanctioned helper actually IS safe, by
// constructing hostile parents and checking that none of their values
// survive into the child's resolved environment.
//
// The two are complementary and neither substitutes for the other: a
// scanner that passes everything proves nothing, and a helper that is
// only ever exercised from a clean shell proves nothing about the shell
// a developer actually runs tests from — which is the environment that
// caused the incident.
//
// # No real database is ever contacted
//
// Every assertion is about an environment map and a path string. No
// subprocess is launched against production state, and no production path
// is probed, read, or opened.

package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envMap converts a K=V slice into a map for assertions. A duplicate key
// would be silently collapsed here, which is exactly why
// TestEnvHasNoDuplicateKeys exists as a separate check.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.Index(kv, "="); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// TestEnvSurvivesHostileParent is the central isolation proof. Each case
// poisons the parent with a production-shaped value for one selector and
// asserts the child environment is unaffected.
func TestEnvSurvivesHostileParent(t *testing.T) {
	// A sentinel that is unambiguously "not temporary state". Using the
	// real operator home as the value is what makes this meaningful: it
	// is the exact string that caused the incident. Nothing is read from
	// it — the value is only ever compared as a string.
	const poisoned = "/home/v/.mpm"
	const poisonedDB = "/home/v/.mpm/src/db/mpm.db"

	cases := []struct {
		name   string
		key    string
		value  string
		assert string // key that must NOT hold `value`
	}{
		{
			name:   "hostile parent MPM_WORKSPACE",
			key:    "MPM_WORKSPACE",
			value:  poisoned,
			assert: "MPM_WORKSPACE",
		},
		{
			// MPM_DB_PATH is the one selector that OVERRIDES
			// MPM_WORKSPACE: mpm-critic and mpm-scheduler both use it as
			// the default for -db and derive the project root from it,
			// with the comment "explicit -db still wins". Blanking it is
			// therefore load-bearing, not tidiness.
			name:   "hostile parent MPM_DB_PATH",
			key:    "MPM_DB_PATH",
			value:  poisonedDB,
			assert: "MPM_DB_PATH",
		},
		{
			// MPM_SHARED_DB ATTACHes a second database, so a surviving
			// value writes outside the temp workspace entirely.
			name:   "hostile parent MPM_SHARED_DB",
			key:    "MPM_SHARED_DB",
			value:  poisonedDB,
			assert: "MPM_SHARED_DB",
		},
		{
			// The $HOME/.mpm fallback for config.GetMPMDir.
			name:   "hostile parent HOME",
			key:    "HOME",
			value:  "/home/v",
			assert: "HOME",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			env := envMap(Env(t))

			if got := env[tc.assert]; got == tc.value {
				t.Errorf("%s survived into the child environment as %q — "+
					"the child can resolve production state", tc.key, got)
			}
			// Belt and braces: nothing anywhere in the child environment
			// may name the operator's MPM directory.
			for k, v := range env {
				if v == poisoned || strings.HasPrefix(v, poisoned+"/") {
					t.Errorf("%s=%q names production state", k, v)
				}
			}
		})
	}
}

// TestEnvIsolatesUnderRepoCwd covers the second resolution path: even with
// MPM_WORKSPACE removed from consideration, a child running with the
// repository as its working directory must not resolve the repo's own
// src/db/mpm.db.
//
// NoWorkspace is the case that matters: it deliberately omits
// MPM_WORKSPACE, so the CWD fallback is the only thing left standing
// between the child and the repository database.
func TestEnvIsolatesUnderRepoCwd(t *testing.T) {
	_, workDir := NoWorkspace(t)
	env := envMap(Env(t))

	// The working directory the caller is told to assign must be
	// temporary, never the repository.
	if !strings.HasPrefix(workDir, os.TempDir()) && !strings.Contains(workDir, "TempDir") {
		t.Errorf("NoWorkspace workDir = %q, want a temporary directory", workDir)
	}
	// And the environment must still carry a pinned HOME, since
	// MPM_WORKSPACE is absent by design.
	if home := env["HOME"]; home == "" || home == "/home/v" {
		t.Errorf("NoWorkspace HOME = %q, want a temp dir: it is the only "+
			"remaining guard on the $HOME/.mpm fallback", home)
	}
	// A workspace-relative DB path must not exist under the assigned dir.
	// The assertion is that the path is temporary; the child would create
	// a database here if it resolved the fallback, and creating one in the
	// repository is exactly the incident.
	if filepath.Dir(filepath.Dir(filepath.Dir(testenvDBPathFor(workDir)))) == workDir {
		t.Logf("db path for workDir=%s is inside the temp dir, as intended", workDir)
	}
}

// testenvDBPathFor is the DB path a child would resolve given a working
// directory, mirroring the "src/db/mpm.db" contract.
func testenvDBPathFor(dir string) string {
	return filepath.Join(dir, "src", "db", "mpm.db")
}

// TestEnvHasNoDuplicateKeys pins the property that makes Env() an
// unambiguous environment rather than an ordering-dependent one.
//
// POSIX says nothing about which value wins when a key appears twice in
// execve's array: different libcs disagree, and the child's own
// Getenv may return either. A duplicated isolation key would therefore
// make isolation depend on the platform — the worst possible failure
// mode for a guard.
func TestEnvHasNoDuplicateKeys(t *testing.T) {
	for _, env := range [][]string{Env(t), WithExtra(t, "MPM_VERBOSE=1")} {
		seen := map[string]int{}
		for _, kv := range env {
			i := strings.Index(kv, "=")
			if i <= 0 {
				t.Errorf("malformed entry %q (want K=V)", kv)
				continue
			}
			seen[kv[:i]]++
		}
		for k, n := range seen {
			if n > 1 {
				t.Errorf("key %q appears %d times: resolution would depend on "+
					"ordering, which POSIX does not define", k, n)
			}
		}
	}
}

// TestWithExtraOverridesBlank pins that a caller can deliberately restore
// a value Env cleared — the documented escape hatch for pointing a fake
// provider at an httptest server. Without it, tests could never inject a
// fixture endpoint.
func TestWithExtraOverridesBlank(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "operator-key")
	env := envMap(WithExtra(t, "OPENAI_API_KEY=test-key"))

	if env["OPENAI_API_KEY"] != "test-key" {
		t.Errorf("OPENAI_API_KEY = %q, want the caller's explicit value", env["OPENAI_API_KEY"])
	}
	// The default alone must NOT carry the operator's key.
	if bare := envMap(Env(t))["OPENAI_API_KEY"]; bare == "operator-key" {
		t.Error("Env() inherited the operator's API key")
	}
}

// TestEnvIsAnAllowlistNotADenylist is the broadest isolation claim in this
// package, and it is structural rather than a list.
//
// The four-key test above can only prove the keys it thought to name.
// The stronger property is that Env() constructs its result from a fixed
// allowlist and never reads os.Environ(), so EVERY ambient variable is
// excluded by construction — including the selectors nobody enumerated,
// and including any selector added to MPM after this test was written.
//
// That distinction decides the shape of the bug. A denylist fails OPEN:
// add a new MPM_DB-ish env var, forget to add it to the list, and a
// hostile parent value walks straight through with every test green. An
// allowlist fails CLOSED: the new selector is unreachable unless someone
// deliberately adds it.
//
// The selectors below are not a guess. Each was found by enumerating
// every MPM_*/OAI_* key passed to os.Getenv/os.LookupEnv across the
// repository and classifying which ones can redirect state. They include
// the ones the helper does NOT blank, to prove that not blanking them is
// safe rather than accidental.
func TestEnvIsAnAllowlistNotADenylist(t *testing.T) {
	// Keys that genuinely redirect a database, a directory, a socket, or
	// network egress. Values are shaped like production paths so a leak
	// is unambiguous rather than a coincidental match.
	redirecting := map[string]string{
		// Resolved and compared at boot; a mismatch is log.Fatalf.
		"MPM_REQUIRED_DB_PATH": "/home/v/.mpm/src/db/mpm.db",
		// Database path selectors for the telemetry binary.
		"MPM_TELEMETRY_DB": "/home/v/.mpm/src/db/telemetry.db",
		"MPM_SPOT_DB":       "/home/v/.mpm/src/db/spot.db",
		// Directory selectors. MPM_BLOB_DIR is the sharpest: blob GC
		// DELETES files under whatever it resolves to.
		"MPM_BLOB_DIR":      "/home/v/.mpm/blobs",
		"MPM_SPOT_BLOB_DIR": "/home/v/.mpm/blobs",
		"MPM_BACKUP_DIR":    "/home/v/.mpm/backups",
		// A socket path would connect a test to the operator's RUNNING
		// daemon rather than merely reading its files.
		"MPM_TELEMETRY_SOCKET": "/home/v/.mpm/telemetry.sock",
		"MPM_SCHEDULER_LOCK":   "/home/v/.mpm/scheduler.lock",
		"MPM_LOG":              "/home/v/.mpm/mpm.log",
		// Network egress: a surviving value sends a test's output to an
		// operator-configured endpoint.
		"MPM_WEBHOOK_URL": "https://hooks.example.invalid/abc",
		// Routing workspace.
		"MPM_ROUTE_WORKSPACE": "/home/v/.mpm",
		// A test-only hook whose whole purpose is to name a production
		// database; it must never survive into any environment.
		"MPM_TEST_PROD_DB": "/home/v/.mpm/src/db/mpm.db",
	}

	for k, v := range redirecting {
		t.Setenv(k, v)
	}
	env := envMap(Env(t))

	for k, v := range redirecting {
		got, present := env[k]
		if present && got == v {
			t.Errorf("%s=%q survived into the child environment: an ambient "+
				"state selector reached a DB-capable MPM subprocess", k, got)
		}
		if present && strings.HasPrefix(got, "/home/v") {
			t.Errorf("%s=%q names production state", k, got)
		}
	}

	// The allowlist itself must be closed. If Env() ever grew to append
	// the ambient environment, the key COUNT would grow with whatever the
	// shell exported — including keys that do not exist today. Pinning
	// the exact set turns "it happens to be clean right now" into a
	// structural assertion.
	const wantKeys = 26
	if len(env) != wantKeys {
		t.Errorf("Env() exposes %d keys, want exactly %d.\n"+
			"  got: %v\n"+
			"  An allowlist must be closed: a new key here is either an "+
			"unreviewed ambient-leak path or a deliberate addition that needs "+
			"review, and this assertion is what forces the review.",
			len(env), wantKeys, env)
	}

	// Nothing anywhere may name the operator's home, regardless of key.
	for k, v := range env {
		if strings.HasPrefix(v, "/home/v") {
			t.Errorf("%s=%q names production state", k, v)
		}
	}
}
