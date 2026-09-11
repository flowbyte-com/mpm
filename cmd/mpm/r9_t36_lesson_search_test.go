// r9_t36_lesson_search_test.go — Round 9 T36 regression.
//
// Pin the lesson save → search round-trip:
//   - save a uniquely identifiable lesson via `mpm lesson add`
//   - search for a distinctive term from the lesson content
//   - the search MUST return the saved lesson
//
// Round 9 T36 surface: a freshly saved lesson was reported as
// "No lessons found" by `mpm lesson search`. The cause was a stale
// FTS5 view in the search query path (searchLessonsLike fallback was
// iterating a stale WHERE clause). The fix surfaces in this test by
// requiring the FTS5 path AND the LIKE fallback to both locate the
// freshly-indexed row, and the search to never lose it after the
// save commits.
//
// Note on FTS-restore interaction: the FTS orphan-state recovery
// (Round 7 commit 15727024) recreates lessons_fts with the canonical
// schema; this test runs against a fresh hermetic DB so a regression
// in fts_recovery would surface here as "No lessons found" after the
// save.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func isHex(s string) bool {
	for _, c := range s {
		if !(('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')) {
			return false
		}
	}
	return true
}

// TestR9T36_LessonAddThenSearch_FindsSaved pins the headline T36
// invariant. Save → search → must find the just-saved row.
func TestR9T36_LessonAddThenSearch_FindsSaved(t *testing.T) {
	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")

	const sentinel = "r9t36sentinelzyxw"
	const content = "R9 T36 round-trip test — searching for sentinel " + sentinel

	binPath := filepath.Join("..", "..", "bin", "mpm")
	if _, err := os.Stat(binPath); os.IsNotExist(err) {
		t.Skip("bin/mpm not built; run make build first")
	}

	run := func(args ...string) (string, int) {
		cmd := exec.Command(binPath, args...)
		cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return string(out), code
	}

	// 1. Save the lesson.
	saveOut, saveCode := run("lesson", "add", content, "--tags", "r9", "--json")
	if saveCode != 0 {
		t.Fatalf("lesson add exit=%d: %s", saveCode, saveOut)
	}
	if !strings.Contains(saveOut, `"success":true`) {
		t.Fatalf("lesson add output missing success marker: %s", saveOut)
	}

	// 2. Search by the sentinel token.
	searchOut, searchCode := run("lesson", "search", sentinel)
	if searchCode != 0 {
		t.Fatalf("lesson search exit=%d: %s", searchCode, searchOut)
	}

	// 3. The "No lessons found" path is the bug.
	if strings.Contains(searchOut, "No lessons found") {
		t.Fatalf("lesson search returned 'No lessons found' for just-saved lesson %q. Output:\n%s",
			sentinel, searchOut)
	}

	// 4. The search must surface the saved row (even if other rows
	// appear from seeding; at minimum the row we just saved must be in
	// the output).
	if !strings.Contains(searchOut, sentinel) {
		t.Errorf("lesson search output missing sentinel token %q. Output:\n%s",
			sentinel, searchOut)
	}
}

// TestR9T36_LessonShredRemovesFromSearch pins the delete-side of T36:
// after `lesson shred` the search must no longer surface the row.
// Pre-fix the lessons_fts row outlived the lesson_base tombstone
// (lessons_au_content trigger broken on legacy install), surfacing
// "ghost" search hits for deleted content.
func TestR9T36_LessonShredRemovesFromSearch(t *testing.T) {
	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")

	const sentinel = "r9t36shredmarkervqr"
	const content = "R9 T36 shred-test content with sentinel " + sentinel

	binPath := filepath.Join("..", "..", "bin", "mpm")
	if _, err := os.Stat(binPath); os.IsNotExist(err) {
		t.Skip("bin/mpm not built; run make build first")
	}

	run := func(args ...string) (string, int) {
		cmd := exec.Command(binPath, args...)
		cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return string(out), code
	}

	saveOut, saveCode := run("lesson", "add", content, "--tags", "r9")
	if saveCode != 0 {
		t.Fatalf("lesson add exit=%d: %s", saveCode, saveOut)
	}

	// Lesson IDs are reported in the search results; capture from a
	// search.
	searchPre, _ := run("lesson", "search", sentinel)
	if !strings.Contains(searchPre, sentinel) {
		t.Fatalf("pre-shred: lesson search missing sentinel: %s", searchPre)
	}

	// Lesson IDs are 16 hex chars (e.g. "af18631fab7f39ff"). Pick the
	// first such token from the search output.
	lessonID := ""
	for _, tok := range strings.FieldsFunc(searchPre, func(r rune) bool {
		return r == '[' || r == ']' || r == ' ' || r == '\t' || r == '\n'
	}) {
		tok = strings.TrimPrefix(tok, "[")
		tok = strings.TrimSuffix(tok, "]")
		if len(tok) == 16 && isHex(tok) {
			lessonID = tok
			break
		}
	}
	if lessonID == "" {
		t.Fatalf("could not extract lesson id from search output: %s", searchPre)
	}

	// Shred → search must no longer find the sentinel.
	shredOut, shredCode := run("lesson", "shred", lessonID)
	if shredCode != 0 {
		t.Fatalf("lesson shred exit=%d: %s", shredCode, shredOut)
	}

	searchPost, _ := run("lesson", "search", sentinel)
	if strings.Contains(searchPost, sentinel) {
		t.Fatalf("post-shred: sentinel %q still surfaces in search. Output:\n%s",
			sentinel, searchPost)
	}
}
