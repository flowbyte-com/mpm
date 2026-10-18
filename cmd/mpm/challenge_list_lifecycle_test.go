// challenge_list_lifecycle_test.go — HAL-required lifecycle contract
// for `mpm challenge list` (issue #20).
//
// The handler resolves its database through getDBReadOnly()
// rather than getDB(). The read-only constructor is fail-closed:
// it does not create the database, does not create the lock file,
// and does not initialize the schema. This file pins both halves
// of that contract end-to-end through the public-CLI handler
// surface:
//
//   - existing DB: the handler must not modify the database
//     file. A SHA snapshot of the file before and after the
//     handler runs must be byte-equal.
//   - missing DB: the handler must not create the database.
//     The DB file must remain absent after the handler runs,
//     and the handler must surface a clear remediation hint
//     to the operator (not a crash, not a silent no-op).
//
// Both tests use real on-disk workspaces. They do NOT use
// installTestDMRouter (which sets up an existing DB) or
// NewTestDM (which is in-memory); those helpers exist to make
// the listing test surface ergonomic, but they short-circuit
// exactly the lifecycle property this file pins.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIssue20_HandleChallengeList_ExistingDB_NotModified —
// HAL defect 1 (post #20). The previous handler called getDB(),
// which routed through NewDatabaseManager. That constructor
// performs schema initialization and baseline seeding on first
// use — both are writes, and the discovery surface is meant to
// be observational. The fix moves the handler to the read-only
// lifecycle.
//
// This test pins the "no modification" half of the contract:
// against a pre-existing on-disk DB, the listing call must not
// write anything back. The test:
//
//  1. Sets MPM_WORKSPACE to a temp dir.
//  2. Builds a real on-disk DB via the writable constructor
//     (this is the "test setup" — equivalent to an operator
//     who has already run a write command).
//  3. Computes a SHA of the DB file.
//  4. Invokes handleChallengeList(["list"]) — the full public
//     CLI surface the operator types.
//  5. Recomputes the SHA and asserts byte equality.
//
// It also runs the call twice (the second invocation exercises
// the read-only handle's "open an already-open DB" path) and
// asserts the SHA is still byte-equal afterwards.
func TestIssue20_HandleChallengeList_ExistingDB_NotModified(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	// Test setup: build a real on-disk DB. This is equivalent
	// to an operator who has run a write command on the
	// workspace. The writable constructor creates the lock
	// file, the schema, and the canonical db file.
	dm, err := mpminternal.NewDatabaseManager(workspace)
	require.NoError(t, err, "writable setup constructor: %v", err)
	require.NoError(t, dm.Close(), "close writable setup DM")

	dbPath := mpminternal.ActiveDBPath(workspace)
	require.FileExists(t, dbPath, "precondition: writable setup must create the DB file")

	// SHA of the DB file before the handler runs. Every byte
	// of the file is hashed — schema, metadata, journal, all
	// of it. A write of any kind (an UPDATE, a row-level
	// trigger, an accidental fts5 reindex on read) would
	// change the SHA.
	before := sha256DBFile(t, dbPath)

	// First invocation: open read-only, list 0 rows, close.
	// The test uses captureStdout + captureBoth via
	// handleChallengeList directly so the assertion can also
	// check the operator-facing output without going through
	// the router. The full router path is exercised by the
	// existing tests in challenge_list_router_test.go; this
	// file pins the lifecycle contract.
	var code int
	captureBoth(t, func() {
		code = handleChallengeList([]string{"list"})
	})
	assert.Equal(t, 0, code, "listing against an existing DB must exit 0")

	// DB file must be byte-equal.
	after := sha256DBFile(t, dbPath)
	assert.Equal(t, before, after,
		"handleChallengeList must not modify the DB file. before=%s after=%s",
		before, after)

	// Second invocation: a fresh read-only handle against the
	// same DB. Asserts the lifecycle is idempotent — the
	// read-only handle does not lock, journal, or otherwise
	// mutate the file across invocations.
	var code2 int
	captureBoth(t, func() {
		code2 = handleChallengeList([]string{"list"})
	})
	assert.Equal(t, 0, code2)
	after2 := sha256DBFile(t, dbPath)
	assert.Equal(t, before, after2,
		"a second handleChallengeList invocation must not modify the DB file. before=%s after=%s",
		before, after2)
}

// TestIssue20_HandleChallengeList_MissingDB_NotInitialized —
// HAL defect 1 (post #20). The "no initialization" half of the
// contract: against an empty workspace with no DB, the listing
// call must not create the database.
//
// The previous getDB() path would have created the database on
// first use (the constructor lazily initializes the schema and
// runs baseline seeding). The read-only lifecycle must instead
// refuse: the DB file is absent, the constructor returns
// ErrReadOnlyDatabaseNotFound, and the handler renders a
// remediation hint instead of silently creating the file.
//
// The test asserts:
//
//  1. Precondition: the DB file does not exist and the parent
//     directory does not exist either (a fresh install that
//     has never run a write command).
//  2. handleChallengeList(["list"]) returns a non-zero exit
//     code and surfaces a clear "no database found, run a
//     write command" message on stderr.
//  3. Post-condition: the DB file is STILL absent, and the
//     parent directory is STILL absent. The handler did not
//     create anything.
func TestIssue20_HandleChallengeList_MissingDB_NotInitialized(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	dbPath := mpminternal.ActiveDBPath(workspace)
	lockPath := mpminternal.MaintenanceLockPath(workspace)
	parentDir := filepath.Dir(dbPath)

	// Precondition: nothing on disk.
	require.NoFileExists(t, dbPath, "precondition: DB file must not exist")
	require.NoFileExists(t, lockPath, "precondition: lock file must not exist")
	require.NoDirExists(t, parentDir, "precondition: parent directory must not exist")

	// Invoke the handler. The read-only constructor checks
	// the lock first (the coordination primitive), then the
	// DB file. On a fresh install with nothing on disk, the
	// lock check fires first and the constructor returns
	// ErrCoordinatedInspectionUnavailable. The handler must
	// surface a clear error and exit non-zero, without having
	// created anything on disk — the contract is "the DB
	// must not be initialized", not "the DB-not-found error
	// is the only acceptable one". The test below accepts
	// either the lock-absent or the DB-not-found message,
	// because both indicate the same operator-facing outcome:
	// "run a write command first, then retry".
	var code int
	out := captureBoth(t, func() {
		code = handleChallengeList([]string{"list"})
	})

	assert.NotEqual(t, 0, code, "listing against a missing DB must exit non-zero; output:\n%s", out)
	assert.True(t,
		strings.Contains(out, "no MPM database found") || strings.Contains(out, "maintenance lock is absent"),
		"operator-facing error must be a clear remediation hint — either the "+
			"DB-not-found message or the lock-absent message is acceptable on a "+
			"fresh install; output:\n%s", out)

	// Post-condition: still nothing on disk. The handler
	// must not have created the DB, the lock, or the parent
	// directory.
	assert.NoFileExists(t, dbPath,
		"handleChallengeList must not create the DB file when none exists")
	assert.NoFileExists(t, lockPath,
		"handleChallengeList must not create the lock file when none exists")
	assert.NoDirExists(t, parentDir,
		"handleChallengeList must not create the parent directory when none exists")
}

// sha256DBFile returns the hex-encoded SHA-256 of a file's
// contents. Used by the no-modification test to assert that
// the DB file is byte-equal before and after a handler call.
// Errors are fatal — the test cannot make any meaningful
// assertion about a file the helper cannot read.
//
// Named sha256DBFile (rather than the generic sha256File
// already used in handlers_restore_safety_test.go) to keep
// the helper scoped to this file's intent: the SHA is being
// taken specifically of the MPM database file as evidence of
// the read-only lifecycle contract, not as a generic
// integrity probe.
func sha256DBFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("sha256DBFile: open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("sha256DBFile: copy %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
