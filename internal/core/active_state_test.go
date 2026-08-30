// active_state_test.go — pin the router-fallback contract:
//   - requested name exists on disk → return it
//   - requested name missing → fall back to "default", log to audit
//   - requested empty → fall back to "default"
//   - both missing → empty string + INFO-level audit (alpha-4 D-001/W-002:
//     expected bootstrap, not an operational fault; gated from the
//     audit-summary error/warning count and from cluster upserts)

package internal

import (
	"os"
	"path/filepath"
	"strings"
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

// TestResolveActivePersona_FreshInstall_LogsInfoNotError (alpha-4 D-001/W-002)
// pins the audit severity on the both-missing branch. Pre-fix this was
// AuditError and surfaced in the wake-context audit summary as "1 errors"
// on every fresh install — a false-positive operational failure. The fix
// demoted the severity to AuditInfo so:
//   - the headline `N errors, M warnings` stays accurate
//     (audit_summary SQL excludes info rows — wake_context.go:933-945)
//   - the cluster-upsert gate stays silent on fresh-install runs
//     (audit.go:138 — only AuditWarn/Error/Fatal/Critical upsert)
//
// This test uses a real *DatabaseManager (existing TestResolveActive_*
// tests pass nil and therefore never asserted on the audit-row side
// effect). Regression pin: a fresh-install persona lookup writes exactly
// one router-component audit row, level=info, with the documented
// bootstrap message.
func TestResolveActivePersona_FreshInstall_LogsInfoNotError(t *testing.T) {
	root := t.TempDir()
	overrideMPMDir(t, root)
	dm := NewTestDM(t)

	// No persona files on disk — both branches will fall through to
	// the both-missing path. Call with a non-empty requested name so
	// the "requested missing → fallback" sibling branch fires first
	// (logging AuditInfo "falling back") and THEN the both-missing
	// branch fires for "default" — the new regression target.
	got := ResolveActivePersona(dm, "anything")
	if got != "" {
		t.Fatalf("expected empty string for both-missing, got %q", got)
	}

	// Query the audit ledger. The both-missing branch is the LAST
	// audit row written for this call (the sibling-fallback row goes
	// in first). We look for ANY row whose message contains the
	// both-missing text — the regression asserts its level is info,
	// not error.
	row := dm.SQLDB().QueryRow(`
		SELECT level, component, message
		FROM system_audit_log
		WHERE component = 'router'
		  AND message LIKE '%also missing on disk%'
		ORDER BY created_at DESC
		LIMIT 1`)

	var level, component, message string
	if err := row.Scan(&level, &component, &message); err != nil {
		t.Fatalf("expected a system_audit_log row for both-missing persona; got %v", err)
	}

	if level != string(AuditInfo) {
		t.Errorf("alpha-4 D-001 regression: fresh-install persona fallback must log at level=info, got level=%q (component=%q, message=%q)", level, component, message)
	}

	// Sanity: ensure AuditSummary() does NOT count this row in the
	// error headline. Pre-fix the summary said "1 errors" on every
	// fresh install. Post-fix it should say "0 errors, 0 warnings".
	summary := dm.AuditSummary()
	if strings.Contains(summary, "1 errors") || strings.Contains(summary, "1 error") {
		t.Errorf("audit summary leaked fresh-install fallback as an error: %q", summary)
	}
}
