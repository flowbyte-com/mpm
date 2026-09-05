// resolve_lesson_theory_bounded_regression_test.go — Pass 3 defect C.12.
//
// The 2026-09-05 audit found mpm://lesson/<id> and mpm://theory/<id>
// in the CLI fallback (handlers.go:5516-5549) hard-coded
// `"bounded": false` regardless of actual content length vs
// max_bytes. The MCP resolver path correctly computes
// `bounded = len(content) > maxBytes`.
//
// Pre-fix reproduction:
//
//   $ mpm call mpm_resolve --payload '{"uri":"mpm://lesson/<id>","max_bytes":50}'
//     # content 596 bytes, max_bytes 50 → should be bounded=true
//     # actual: bounded=false (lying to the consumer)
//
// Fix: compute `bounded` from len(content) > maxBytes for both
// lesson and theory cases, matching the work case at handlers.go:5563
// and the MCP resolver path at cmd/mpm-mcp/tools.go:207/:250.
//
// The CLI fallback is the path exercised by `mpm call mpm_resolve`
// (i.e., the schema-aware dispatch in handlers.go) — distinct from
// the MCP resolver which routes through tools.go. The two paths must
// produce the same bounded semantics.

package tools

import (
	"strings"
	"testing"
)

// TestResolveLesson_CliFallbackHonorsMaxBytes pins that the CLI
// fallback for mpm://lesson/<id> sets bounded=true when the content
// exceeds max_bytes, matching the MCP resolver semantics.
func TestResolveLesson_CliFallbackHonorsMaxBytes(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := defaultACForPatch()

	// Force the CLI fallback path by clearing the global resolver
	// for the duration of this test.
	saved := globalResolver
	globalResolver = nil
	t.Cleanup(func() { globalResolver = saved })

	// Seed a lesson directly. The lessons view reads from
	// lessons_base; we write the underlying row.
	longContent := make([]byte, 0, 600)
	for len(longContent) < 600 {
		longContent = append(longContent, []byte("the quick brown fox jumps over the lazy dog. ")...)
	}
	lessonID := "lesson-bounded-probe-" + sanitizeForPointerURI(t.Name())
	_, err := dm.SQLDB().Exec(`
		INSERT INTO lessons_base (id, type, content, tags, reinforcement_count, created)
		VALUES (?, 'insight', ?, '[]', 1, CAST(strftime('%s','now') AS TEXT))
	`, lessonID, string(longContent))
	if err != nil {
		t.Fatalf("seed lesson: %v", err)
	}

	res, err := handleMpmResolve(dm, ac, map[string]interface{}{
		"uri":       "mpm://lesson/" + lessonID,
		"max_bytes": float64(50),
	})
	if err != nil {
		t.Fatalf("CLI fallback lesson resolve: %v", err)
	}
	m, _ := res.(map[string]interface{})
	bounded, _ := m["bounded"].(bool)
	content, _ := m["content"].(string)
	if !bounded {
		t.Errorf("lesson content (len=%d) with max_bytes=50 must be bounded=true", len(content))
	}
}

// TestResolveTheory_CliFallbackHonorsMaxBytes pins the same contract
// for mpm://theory/<id>.
func TestResolveTheory_CliFallbackHonorsMaxBytes(t *testing.T) {
	dm := newTestSharedDM(t)
	ac := defaultACForPatch()

	// Force the CLI fallback path.
	saved := globalResolver
	globalResolver = nil
	t.Cleanup(func() { globalResolver = saved })

	// Seed a theory (collection='theories'). The CLI fallback
	// resolves theories via dm.GetMemory + collection check.
	longContent := make([]byte, 0, 600)
	for len(longContent) < 600 {
		longContent = append(longContent, []byte("the quick brown fox jumps over the lazy dog. ")...)
	}
	theoryID := "theory-bounded-probe-" + sanitizeForPointerURI(t.Name())
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, tags, metadata, deleted_at, created_at, updated_at)
		VALUES (?, 'theories', ?, 5, '[]', '{}', NULL, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
	`, theoryID, string(longContent))
	if err != nil {
		t.Fatalf("seed theory: %v", err)
	}

	res, err := handleMpmResolve(dm, ac, map[string]interface{}{
		"uri":       "mpm://theory/" + theoryID,
		"max_bytes": float64(50),
	})
	if err != nil {
		t.Fatalf("CLI fallback theory resolve: %v", err)
	}
	m, _ := res.(map[string]interface{})
	bounded, _ := m["bounded"].(bool)
	content, _ := m["content"].(string)
	if !bounded {
		t.Errorf("theory content (len=%d) with max_bytes=50 must be bounded=true", len(content))
	}
}

// --- helpers ---

// sanitizeForPointerURI returns a pointer-id-safe slug derived from
// the test name (the parser at handlers.go:5661 rejects anything
// outside [a-z0-9-]).
func sanitizeForPointerURI(name string) string {
	out := make([]byte, 0, len(name))
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out = append(out, byte(r))
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}
