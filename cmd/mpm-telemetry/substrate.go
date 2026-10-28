// cmd/mpm-telemetry/substrate.go — the sidecar's read-only window onto
// the MPM substrate.
//
// WHY IT EXISTS. The HighTokenNoArtifactHunt has to decide whether a
// session produced anything durable in MPM, and the substrate's
// artifact_provenance table is the only source for that answer.
//
// It used to shell out to `mpm call mpm_provenance --payload
// {"action":"count_by_session"}`. TWO things were wrong with that:
//
//  1. THE TOOL DOES NOT EXIST. The registry publishes 21 tools and no
//     mpm_provenance, so the hunt failed on every qualifying session
//     ("mpm_provenance: exit status 1") and the command exited 1.
//  2. EVEN IF IT EXISTED, IT COULD NOT HAVE BEEN READ-ONLY. Every
//     `mpm call` writes a tool_invocations row into mpm.db — including
//     a call that only reads. A subprocess-based count can therefore
//     never be a read-only count, which is what the dry run promises.
//
// So the count is computed IN-PROCESS, against a handle opened with
// SQLite's mode=ro. Nothing is written — not to mpm.db, not to
// telemetry.db, not anywhere. This is the same shape the critic's
// telemetry hunt uses since #138, and it uses the same attribution
// rule (internal/attribution), so the two cannot drift.

package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// errSubstrateAbsent reports that the substrate's database file does
// not exist at the resolved path.
var errSubstrateAbsent = errors.New("substrate database not found")

// substrateDBPath resolves the substrate's database path.
//
// MPM_DB_PATH wins — it is the same override the scheduler honours
// (internal/scheduler.defaultDBPath). Otherwise the path is
// workspace-relative: <workspace>/src/db/mpm.db.
//
// It is NEVER cwd-relative. The ghost-DB shape this codebase removed
// after the 2026-07-21 incident (lesson 59fe3f8ff3e1549e) made a
// reader resolve a different substrate depending on where the process
// was started, so a diagnostic could compare a session against the
// wrong store.
func substrateDBPath(workspace string) (string, error) {
	if p := strings.TrimSpace(os.Getenv("MPM_DB_PATH")); p != "" {
		return filepath.Clean(p), nil
	}
	if strings.TrimSpace(workspace) == "" {
		return "", errors.New(
			"cannot resolve the substrate database path: no workspace was given — " +
				"set MPM_WORKSPACE, or set MPM_DB_PATH to the substrate's path")
	}
	return filepath.Join(workspace, "src", "db", "mpm.db"), nil
}

// openSubstrateReadOnly opens the substrate's database for reading.
//
// A missing database is reported as errSubstrateAbsent rather than
// created: a diagnostic must not bring a substrate into existence.
// An existing database with no schema is an error, not a silent zero:
// "the substrate has no tool_invocations table" and "this session
// produced nothing" are different claims.
func openSubstrateReadOnly(path string) (*sql.DB, error) {
	clean := filepath.Clean(path)
	if _, err := os.Stat(clean); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", errSubstrateAbsent, clean)
		}
		return nil, fmt.Errorf("stat substrate database %s: %w", clean, err)
	}
	abs, err := filepath.Abs(clean)
	if err != nil {
		return nil, fmt.Errorf("resolve substrate database %s: %w", clean, err)
	}
	uri := (&url.URL{
		Scheme:   "file",
		Path:     abs,
		RawQuery: "mode=ro&_busy_timeout=5000",
	}).String()
	db, err := sql.Open("sqlite3", uri)
	if err != nil {
		return nil, fmt.Errorf("open substrate read-only: %w", err)
	}
	// Prove the handle works before the hunt depends on it.
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM tool_invocations").Scan(&n); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read substrate %s: %w "+
			"(has the substrate been initialised? run `mpm init`)", clean, err)
	}
	return db, nil
}
