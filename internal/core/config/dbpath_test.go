// dbpath_test.go — the database-path resolver must never silently open a
// database the operator did not name.
//
// THE DEFECT THIS PINS
//
// mpm-scheduler and mpm-critic derived the workspace from an explicit
// --db / MPM_DB_PATH with Dir(Dir(Dir(path))) — the assumption being
// that the path always has the canonical <root>/src/db/mpm.db shape.
// The shape was ASSUMED, never CHECKED, so:
//
//	MPM_DB_PATH=/tmp/other.db   →  workspace "/"  →  DB "/src/db/mpm.db"
//	MPM_DB_PATH=/srv/data/mpm.db →  workspace "/"  →  DB "/src/db/mpm.db"
//
// The operator asked for one database and got another, with no message
// at startup or ever afterwards.
//
// THE RULE UNDER TEST (decision D4)
//
// MPM_WORKSPACE is the canonical workspace selector. MPM_DB_PATH may
// only RESTATE the workspace's own database; anything else is an
// explicit conflict error.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withWorkspace points MPM_WORKSPACE at a disposable directory and
// clears MPM_DB_PATH, so each test states its own override.
func withWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", "")
	return ws
}

// TestResolveTarget_UnsetOverrideUsesTheWorkspace is the baseline: with
// no override, the workspace's own database is opened.
func TestResolveTarget_UnsetOverrideUsesTheWorkspace(t *testing.T) {
	ws := withWorkspace(t)
	gotWS, gotDB, err := ResolveTarget("")
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if gotWS != ws {
		t.Errorf("workspace = %q, want %q", gotWS, ws)
	}
	want := filepath.Join(ws, "src", "db", "mpm.db")
	if gotDB != want {
		t.Errorf("db path = %q, want %q", gotDB, want)
	}
}

// TestResolveTarget_OverrideRestatingTheWorkspaceIsAccepted pins that a
// consistent override still works — the live systemd unit sets exactly
// this pair (MPM_WORKSPACE=/home/v/.mpm,
// MPM_DB_PATH=/home/v/.mpm/src/db/mpm.db), and it must keep working.
func TestResolveTarget_OverrideRestatingTheWorkspaceIsAccepted(t *testing.T) {
	ws := withWorkspace(t)
	canonical := filepath.Join(ws, "src", "db", "mpm.db")
	t.Setenv("MPM_DB_PATH", canonical)

	gotWS, gotDB, err := ResolveTarget("")
	if err != nil {
		t.Fatalf("a consistent MPM_DB_PATH must be accepted: %v", err)
	}
	if gotWS != ws || gotDB != canonical {
		t.Errorf("got (%q, %q), want (%q, %q)", gotWS, gotDB, ws, canonical)
	}
}

// TestResolveTarget_DivergentOverrideIsAnExplicitError is the headline
// regression: the path that used to be silently rewritten.
//
// The error must name BOTH paths — the operator's next action depends
// on which one they meant.
func TestResolveTarget_DivergentOverrideIsAnExplicitError(t *testing.T) {
	ws := withWorkspace(t)
	divergent := "/tmp/other.db"
	t.Setenv("MPM_DB_PATH", divergent)

	_, _, err := ResolveTarget("")
	if err == nil {
		t.Fatal("a divergent MPM_DB_PATH must be an error, not a silent redirection")
	}
	if !IsDBPathConflict(err) {
		t.Errorf("the error must be a DBPathConflictError; got %T: %v", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, divergent) {
		t.Errorf("the error must name the override %q; got: %s", divergent, msg)
	}
	canonical := filepath.Join(ws, "src", "db", "mpm.db")
	if !strings.Contains(msg, canonical) {
		t.Errorf("the error must name the canonical database %q; got: %s", canonical, msg)
	}
	// And it must say what to DO.
	if !strings.Contains(msg, "MPM_WORKSPACE") {
		t.Errorf("the error must point at the supported selector; got: %s", msg)
	}
}

// TestResolveTarget_ExplicitNonCanonicalPathIsRejected pins the
// scheduler's and the critic's own --db flag.
//
// `--db /tmp/other.db` used to mean "open /src/db/mpm.db". It now
// fails, loudly, at startup.
func TestResolveTarget_ExplicitNonCanonicalPathIsRejected(t *testing.T) {
	withWorkspace(t)

	_, _, err := ResolveTarget("/tmp/other.db")
	if err == nil {
		t.Fatal("a --db without the canonical shape must be rejected")
	}
	if !IsDBPathConflict(err) {
		t.Errorf("the error must be a DBPathConflictError; got %T: %v", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "/tmp/other.db") {
		t.Errorf("the error must name the rejected path; got: %s", msg)
	}
	if !strings.Contains(msg, "src/db/mpm.db") {
		t.Errorf("the error must state the canonical shape; got: %s", msg)
	}
}

// TestResolveTarget_ExplicitCanonicalPathSelectsItsWorkspace pins the
// supported override: a --db that HAS the canonical shape names a
// workspace, and that workspace is used.
func TestResolveTarget_ExplicitCanonicalPathSelectsItsWorkspace(t *testing.T) {
	withWorkspace(t) // MPM_WORKSPACE set — cleared below for this case
	t.Setenv("MPM_WORKSPACE", "")

	other := t.TempDir()
	db := filepath.Join(other, "src", "db", "mpm.db")

	gotWS, gotDB, err := ResolveTarget(db)
	if err != nil {
		t.Fatalf("a canonical-shape --db must be accepted: %v", err)
	}
	if gotWS != other {
		t.Errorf("workspace = %q, want %q", gotWS, other)
	}
	if gotDB != db {
		t.Errorf("db path = %q, want %q", gotDB, db)
	}
}

// TestResolveTarget_ExplicitPathAndWorkspaceDisagreeIsAConflict pins
// the two-selector case: an explicit --db naming one workspace while
// MPM_WORKSPACE names another.
func TestResolveTarget_ExplicitPathAndWorkspaceDisagreeIsAConflict(t *testing.T) {
	ws := withWorkspace(t)
	other := t.TempDir()
	db := filepath.Join(other, "src", "db", "mpm.db")

	_, _, err := ResolveTarget(db)
	if err == nil {
		t.Fatal("two selectors naming two workspaces must be a conflict")
	}
	msg := err.Error()
	if !strings.Contains(msg, ws) || !strings.Contains(msg, other) {
		t.Errorf("the error must name both workspaces (%q and %q); got: %s", ws, other, msg)
	}
}

// TestResolveTarget_ExplicitPathAndEnvOverrideDisagreeIsAConflict
// pins the other two-selector case: an explicit --db and an MPM_DB_PATH
// naming different databases.
func TestResolveTarget_ExplicitPathAndEnvOverrideDisagreeIsAConflict(t *testing.T) {
	withWorkspace(t)
	other := t.TempDir()
	db := filepath.Join(other, "src", "db", "mpm.db")
	t.Setenv("MPM_DB_PATH", filepath.Join(t.TempDir(), "src", "db", "mpm.db"))

	_, _, err := ResolveTarget(db)
	if err == nil {
		t.Fatal("--db and MPM_DB_PATH naming different databases must be a conflict")
	}
}

// TestResolveTarget_SymlinkedWorkspaceIsTheSameDatabase pins that the
// escape hatch works: a workspace whose canonical database is a symlink
// to a database at a non-canonical path is recognised as naming that
// database, so the override is accepted rather than reported as a
// conflict.
//
// This is the documented way to point MPM at a recovered database that
// lives outside the canonical layout.
func TestResolveTarget_SymlinkedWorkspaceIsTheSameDatabase(t *testing.T) {
	// The database lives outside the canonical layout…
	elsewhere := t.TempDir()
	realDB := filepath.Join(elsewhere, "recovered.db")
	if err := os.WriteFile(realDB, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// …and the workspace's canonical slot symlinks to it.
	ws := t.TempDir()
	dbDir := filepath.Join(ws, "src", "db")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dbDir, "mpm.db")
	if err := os.Symlink(realDB, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", realDB)

	_, _, err := ResolveTarget("")
	if err != nil {
		t.Fatalf("a symlinked workspace database must be accepted: %v", err)
	}
}

// TestHasCanonicalDBShape pins the shape predicate directly.
func TestHasCanonicalDBShape(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/home/v/.mpm/src/db/mpm.db", true},
		{filepath.Join("relative", "src", "db", "mpm.db"), true},
		{"/tmp/other.db", false},
		{"/srv/data/mpm.db", false},
		{"/home/v/.mpm/src/db/other.db", false},
		{"/home/v/.mpm/db/mpm.db", false},
		{"/home/v/.mpm/src/mpm.db", false},
		{"/src/db/mpm.db", false}, // a workspace at the filesystem root
		{"", false},
	}
	for _, c := range cases {
		if got := HasCanonicalDBShape(c.path); got != c.want {
			t.Errorf("HasCanonicalDBShape(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestWorkspaceRootForDBPath pins the root derivation, including the
// degenerate roots that must be refused.
func TestWorkspaceRootForDBPath(t *testing.T) {
	root, ok := WorkspaceRootForDBPath("/home/v/.mpm/src/db/mpm.db")
	if !ok || root != "/home/v/.mpm" {
		t.Errorf("WorkspaceRootForDBPath = (%q, %v), want (/home/v/.mpm, true)", root, ok)
	}
	if _, ok := WorkspaceRootForDBPath("/src/db/mpm.db"); ok {
		t.Error("a workspace at the filesystem root must be refused")
	}
	if _, ok := WorkspaceRootForDBPath("/tmp/other.db"); ok {
		t.Error("a non-canonical path must have no workspace root")
	}
}

// TestCanonicalDBPath pins the canonical shape in one place.
func TestCanonicalDBPath(t *testing.T) {
	got := CanonicalDBPath("/srv/mpm")
	want := filepath.Join("/srv", "mpm", "src", "db", "mpm.db")
	if got != want {
		t.Errorf("CanonicalDBPath = %q, want %q", got, want)
	}
}
