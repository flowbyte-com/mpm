package attribution

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// The attribution rule — "how many durable artifacts did this framework
// session produce?" — had copies. The critic's telemetry hunt carried
// one, the missed-opportunity hunt carried a second, and the telemetry
// sidecar needed a third.
//
// Copies of a rule drift. These tests pin the ONE rule, so a future
// divergence is a test failure rather than a silent disagreement
// between two hunts.
//
// The substrate schema is recreated here in miniature: the rule is
// about two tables and a join, and pinning it against a miniature
// keeps this package free of a dependency on the whole substrate.

// newSubstrate creates a disposable database with the two tables the
// rule reads.
func newSubstrate(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mpm.db")
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	stmts := []string{
		`CREATE TABLE tool_invocations (
			id TEXT PRIMARY KEY,
			invocation_id TEXT,
			framework_session_id TEXT
		)`,
		`CREATE TABLE artifact_provenance (
			id TEXT PRIMARY KEY,
			artifact_id TEXT,
			invocation_id TEXT
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
	return db
}

func seedInvocation(t *testing.T, db *sql.DB, id, invocationID, session string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO tool_invocations (id, invocation_id, framework_session_id) VALUES (?,?,?)`,
		id, invocationID, session); err != nil {
		t.Fatalf("seed tool_invocations: %v", err)
	}
}

func seedArtifact(t *testing.T, db *sql.DB, id, artifactID, invocationID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO artifact_provenance (id, artifact_id, invocation_id) VALUES (?,?,?)`,
		id, artifactID, invocationID); err != nil {
		t.Fatalf("seed artifact_provenance: %v", err)
	}
}

// TestSessionArtifactCount_AttributesThroughTheInvocation is the rule.
func TestSessionArtifactCount_AttributesThroughTheInvocation(t *testing.T) {
	db := newSubstrate(t)
	ctx := context.Background()

	seedInvocation(t, db, "ti-1", "inv-1", "ses-a")
	seedArtifact(t, db, "ap-1", "mem-1", "inv-1")
	seedArtifact(t, db, "ap-2", "mem-2", "inv-1")

	// Two artifacts, attributed through the same invocation.
	if n, err := SessionArtifactCount(ctx, db, "ses-a"); err != nil || n != 2 {
		t.Fatalf("count = %d (err %v), want 2", n, err)
	}

	// A session with invocations but no artifacts.
	seedInvocation(t, db, "ti-2", "inv-2", "ses-b")
	if n, err := SessionArtifactCount(ctx, db, "ses-b"); err != nil || n != 0 {
		t.Fatalf("count = %d (err %v), want 0", n, err)
	}

	// A session that does not exist.
	if n, err := SessionArtifactCount(ctx, db, "ses-unknown"); err != nil || n != 0 {
		t.Fatalf("count = %d (err %v), want 0", n, err)
	}
}

// TestSessionArtifactCount_DoesNotCrossSessions pins that an artifact
// belongs to exactly one session.
func TestSessionArtifactCount_DoesNotCrossSessions(t *testing.T) {
	db := newSubstrate(t)
	ctx := context.Background()

	seedInvocation(t, db, "ti-1", "inv-1", "ses-a")
	seedArtifact(t, db, "ap-1", "mem-1", "inv-1")
	seedInvocation(t, db, "ti-2", "inv-2", "ses-b")

	if n, err := SessionArtifactCount(ctx, db, "ses-b"); err != nil || n != 0 {
		t.Fatalf("count = %d (err %v), want 0; an artifact must not cross sessions", n, err)
	}
}

// TestSessionArtifactCount_CountsDistinctArtifacts pins that the same
// artifact attributed twice counts once.
func TestSessionArtifactCount_CountsDistinctArtifacts(t *testing.T) {
	db := newSubstrate(t)
	ctx := context.Background()

	seedInvocation(t, db, "ti-1", "inv-1", "ses-a")
	seedInvocation(t, db, "ti-2", "inv-2", "ses-a")
	seedArtifact(t, db, "ap-1", "mem-1", "inv-1")
	seedArtifact(t, db, "ap-2", "mem-1", "inv-2")

	if n, err := SessionArtifactCount(ctx, db, "ses-a"); err != nil || n != 1 {
		t.Fatalf("count = %d (err %v), want 1", n, err)
	}
}

// TestSessionArtifactCount_EmptySessionIsZero pins that an empty session
// id is zero, not an error.
func TestSessionArtifactCount_EmptySessionIsZero(t *testing.T) {
	db := newSubstrate(t)
	if n, err := SessionArtifactCount(context.Background(), db, ""); err != nil || n != 0 {
		t.Fatalf("count = %d (err %v), want 0", n, err)
	}
}

// TestSessionArtifactCount_NilDBIsAnError pins that a missing handle is
// a clear error rather than a silent zero — "the count could not be
// taken" and "the session produced nothing" are different claims.
func TestSessionArtifactCount_NilDBIsAnError(t *testing.T) {
	if _, err := SessionArtifactCount(context.Background(), nil, "ses-a"); err == nil {
		t.Fatal("SessionArtifactCount succeeded with a nil handle")
	}
}
