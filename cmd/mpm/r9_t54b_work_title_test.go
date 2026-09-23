// r9_t54b_work_title_test.go — Round 9 T54b regression.
//
// Pin three surfaces for `mpm work item create`:
//   1. positional <title> works (canonical muscle-memory form)
//   2. --title <text> works (ergonomic flag form)
//   3. --title + positional resolves the conflict with positional
//      winning and a non-fatal notice (NOT an error)
//
// Pre-fix T54b: the create branch required positional title and
// never read the parsed --title flag, so `mpm work item create
// --title "X"` got "mpm work item create requires a title
// positional arg" even though `--title` was already in the parser.

package main

import (
	"strings"
	"testing"
)

func r9T54bMpm(t *testing.T, workspace string, args ...string) (string, int) {
	t.Helper()
	return mpmRun(t, mpmCmd(t), workspace, args...)
}

// TestR9T54b_PositionalTitle pins the canonical muscle-memory form.
func TestR9T54b_PositionalTitle(t *testing.T) {
	out, code := r9T54bMpm(t, t.TempDir(),
		"work", "item", "create", "r9-t54b-positional")
	if code != 0 {
		t.Fatalf("positional title should succeed. Output:\n%s", out)
	}
	if !strings.Contains(out, `"title"`) || !strings.Contains(out, "r9-t54b-positional") {
		t.Errorf("response missing title field. Output:\n%s", out)
	}
}

// TestR9T54b_FlagTitle pins the ergonomic --title form. Pre-fix
// this returned non-zero because the create branch ignored the
// flag.
func TestR9T54b_FlagTitle(t *testing.T) {
	out, code := r9T54bMpm(t, t.TempDir(),
		"work", "item", "create", "--title", "r9-t54b-flag-form")
	if code != 0 {
		t.Fatalf("--title form should succeed. Output:\n%s", out)
	}
	if !strings.Contains(out, "r9-t54b-flag-form") {
		t.Errorf("response missing flag title. Output:\n%s", out)
	}
}

// TestR9T54b_FlagBeforeSubcommand pins the leading-flag ergonomic
// (Round 9 Surfacing Spec note): `mpm work item --title X create`
// also works through parseWorkItemArgs' leading-flag path.
func TestR9T54b_FlagBeforeSubcommand(t *testing.T) {
	out, code := r9T54bMpm(t, t.TempDir(),
		"work", "item", "--title", "r9-t54b-leading-flag", "create")
	if code != 0 {
		t.Fatalf("leading-flag form should succeed. Output:\n%s", out)
	}
	if !strings.Contains(out, "r9-t54b-leading-flag") {
		t.Errorf("response missing leading-flag title. Output:\n%s", out)
	}
}

// TestR9T54b_NoTitleRejected pins the negative case: neither
// positional nor --title must surface a clear non-zero error.
func TestR9T54b_NoTitleRejected(t *testing.T) {
	out, code := r9T54bMpm(t, t.TempDir(),
		"work", "item", "create")
	if code == 0 {
		t.Fatalf("create with no title should fail. Output:\n%s", out)
	}
	if !strings.Contains(out, "title") {
		t.Errorf("expected error to mention 'title'. Output:\n%s", out)
	}
}

// TestR9T54b_ConflictPositionalWins pins the conflict-resolution
// contract: when both positional and --title are supplied, positional
// wins AND the call still succeeds (with a notice). The smoke test
// required this for muscle-memory + tooling that bulk-applies.
func TestR9T54b_ConflictPositionalWins(t *testing.T) {
	out, code := r9T54bMpm(t, t.TempDir(),
		"work", "item", "create", "--title", "from-flag", "from-positional")
	if code != 0 {
		t.Fatalf("conflict resolution should succeed (positional wins, notice on stderr). Output:\n%s", out)
	}
	if !strings.Contains(out, "from-positional") {
		t.Errorf("positional value should be the persisted title. Output:\n%s", out)
	}
	if strings.Contains(out, `"title":"from-flag"`) {
		t.Errorf("--title should be shadowed by positional, got:\n%s", out)
	}
}
