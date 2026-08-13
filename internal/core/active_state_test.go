// active_state_test.go — pin the router-fallback contract:
//   - requested name exists on disk → return it
//   - requested name missing → fall back to "default", log to audit
//   - requested empty → fall back to "default"
//   - both missing → empty string + ERROR-level audit

package internal

import (
	"os"
	"path/filepath"
	"testing"
)

// writePersonaModeFile creates a minimal persona/mode .md on disk
// under root for the resolver's existence check. root must be the
// same path passed to overrideMPMDir — otherwise the resolver looks
// in a different directory than the file was written to.
func writePersonaModeFile(t *testing.T, root, kind, name string) {
	t.Helper()
	dir := filepath.Join(root, kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte("---\nname: "+name+"\n---\n"), 0o600); err != nil {
		t.Fatalf("write %s.md: %v", name, err)
	}
}

// overrideMPMDir redirects config.GetMPMDir() to a test-controlled root.
// Used so we can pre-populate or omit the persona/mode files per-test.
func overrideMPMDir(t *testing.T, root string) {
	t.Helper()
	orig := os.Getenv("MPM_WORKSPACE")
	os.Setenv("MPM_WORKSPACE", root)
	t.Cleanup(func() { os.Setenv("MPM_WORKSPACE", orig) })
}

func TestResolveActivePersona_RequestedExists(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "persona", "forensic")
	// 'default' is the fallback target — it must also exist for the
	// happy path to actually fall back. We don't want to test "no
	// fallback fired" here; the no-fallback case is below.
	writePersonaModeFile(t, root, "persona", "default")

	got := ResolveActivePersona(nil, "forensic")
	if got != "forensic" {
		t.Errorf("expected forensic, got %q", got)
	}
}

func TestResolveActivePersona_RequestedMissing_FallsBackToDefault(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	// Only 'default' exists; 'requested' does not.
	writePersonaModeFile(t, root, "persona", "default")

	got := ResolveActivePersona(nil, "does-not-exist")
	if got != "default" {
		t.Errorf("expected fallback to default, got %q", got)
	}
}

func TestResolveActivePersona_Empty_FallsBackToDefault(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "persona", "default")

	got := ResolveActivePersona(nil, "")
	if got != "default" {
		t.Errorf("expected default for empty input, got %q", got)
	}
}

func TestResolveActivePersona_BothMissing_EmptyString(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	// Neither file exists.

	got := ResolveActivePersona(nil, "anything")
	if got != "" {
		t.Errorf("expected empty string when both requested and fallback missing, got %q", got)
	}
}

func TestResolveActiveMode_RequestedMissing_FallsBackToDefault(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "default")

	got := ResolveActiveMode(nil, "missing-mode")
	if got != "default" {
		t.Errorf("expected fallback to default, got %q", got)
	}
}

func TestResolveActiveMode_RequestedExists(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "mode", "debugging")
	writePersonaModeFile(t, root, "mode", "default") // fallback present

	got := ResolveActiveMode(nil, "debugging")
	if got != "debugging" {
		t.Errorf("expected debugging, got %q", got)
	}
}

// TestResolveActive_DMNilSafe verifies the fallback works even when
// the caller can't pass a DatabaseManager (e.g., CLI invocation
// before DB is opened). The audit-log line is skipped but the
// fallback itself still returns the safe default.
func TestResolveActive_DMNilSafe(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	writePersonaModeFile(t, root, "persona", "default")

	got := ResolveActivePersona(nil, "ghost")
	if got != "default" {
		t.Errorf("nil dm should not break fallback; got %q", got)
	}
}
