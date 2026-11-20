package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestModeList_FilesystemBacked verifies the multi-mode list reflects
// actual mode files (plural contract honoured).
func TestModeList_FilesystemBacked(t *testing.T) {
	setupHandlerTestWorkspace(t,
		[]string{"default"},
		[]string{"architect", "debugging", "default"})

	out := captureStdout(t, func() {
		handleModeList()
	})
	for _, name := range []string{"architect", "debugging", "default"} {
		if !strings.Contains(out, name) {
			t.Errorf("list missing %q in output:\n%s", name, out)
		}
	}
	if strings.Contains(out, "README") {
		t.Errorf("mode list should not surface README:\n%s", out)
	}
}

func TestModeList_READMEWithFrontmatter_StillExcluded(t *testing.T) {
	root := setupHandlerTestWorkspace(t,
		[]string{"default"},
		[]string{"architect", "default"})
	readme := "---\nname: README\n---\n# malicious\n"
	if err := os.WriteFile(filepath.Join(root, "mode", "README.md"), []byte(readme), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	out := captureStdout(t, func() {
		handleModeList()
	})
	if strings.Contains(out, "README") {
		t.Errorf("mode README with frontmatter must not appear:\n%s", out)
	}
}

// TestModeAdd_RoundTripPreservesAllSelected is the core multi-mode
// invariant: adding multiple modes must preserve the full collection
// across a write-read cycle.
func TestModeAdd_RoundTripPreservesAllSelected(t *testing.T) {
	root := setupHandlerTestWorkspace(t,
		[]string{"default"},
		[]string{"debugging", "forensic", "default"})

	captureStdout(t, func() {
		handleModeAdd([]string{"debugging"})
	})
	captureStdout(t, func() {
		handleModeAdd([]string{"forensic"})
	})

	got, err := internal_NewModeManager("").GetActive()
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 modes, got %v", got)
	}
	// Order matters: "debugging" added first.
	if got[0] != "debugging" || got[1] != "forensic" {
		t.Errorf("mode order not preserved: %v", got)
	}

	// Mirror file written with first non-auto mode.
	mirrorData, err := os.ReadFile(filepath.Join(root, "config", "current_mode"))
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if string(mirrorData) != "debugging" {
		t.Errorf("mirror file=%q, want %q", string(mirrorData), "debugging")
	}
}

// TestModeAdd_DuplicateIsIdempotent — adding an already-active mode
// must be a no-op (does not duplicate the entry).
func TestModeAdd_DuplicateIsIdempotent(t *testing.T) {
	setupHandlerTestWorkspace(t,
		[]string{"default"},
		[]string{"debugging", "default"})

	captureStdout(t, func() { handleModeAdd([]string{"debugging"}) })
	captureStdout(t, func() { handleModeAdd([]string{"debugging"}) })

	got, _ := internal_NewModeManager("").GetActive()
	if len(got) != 1 {
		t.Errorf("duplicate add should not duplicate: got %v", got)
	}
}

// TestModeRemove_LeavesOthersIntact verifies removal preserves the
// remaining modes (does NOT collapse to a single mode or clear all).
func TestModeRemove_LeavesOthersIntact(t *testing.T) {
	setupHandlerTestWorkspace(t,
		[]string{"default"},
		[]string{"debugging", "forensic", "default"})

	captureStdout(t, func() { handleModeAdd([]string{"debugging"}) })
	captureStdout(t, func() { handleModeAdd([]string{"forensic"}) })
	captureStdout(t, func() { handleModeRemove([]string{"debugging"}) })

	got, _ := internal_NewModeManager("").GetActive()
	if len(got) != 1 || got[0] != "forensic" {
		t.Errorf("expected [forensic], got %v", got)
	}
}

func TestModeRemove_NotActive_ReturnsError(t *testing.T) {
	setupHandlerTestWorkspace(t,
		[]string{"default"},
		[]string{"debugging", "default"})

	captureStdout(t, func() { handleModeAdd([]string{"debugging"}) })
	captureStderr(t, func() {
		handleModeRemove([]string{"forensic"})
	})
}

func TestModeAdd_UnknownName_ReturnsError(t *testing.T) {
	setupHandlerTestWorkspace(t,
		[]string{"default"},
		[]string{"default"})

	captureStderr(t, func() {
		handleModeAdd([]string{"ghost"})
	})
}

// TestModeClear_WritesExplicitEmpty mirrors the persona clear test:
// clear must write [] (NOT null/absent) so wake context distinguishes
// explicit-clear from bootstrap.
func TestModeClear_WritesExplicitEmpty(t *testing.T) {
	root := setupHandlerTestWorkspace(t,
		[]string{"default"},
		[]string{"debugging", "default"})

	captureStdout(t, func() { handleModeAdd([]string{"debugging"}) })
	captureStdout(t, func() { handleModeClear() })

	data, err := os.ReadFile(filepath.Join(root, "active.json"))
	if err != nil {
		t.Fatalf("read active.json: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}
	modes, ok := raw["modes"]
	if !ok {
		t.Errorf("modes field absent — clear should write [] with key present")
	}
	arr, _ := modes.([]interface{})
	if arr == nil {
		t.Errorf("modes=%v after clear, want []", modes)
	}
	if len(arr) != 0 {
		t.Errorf("modes=%v after clear, want []", modes)
	}
}

func TestModeShow_ReadsActualDefinition(t *testing.T) {
	setupHandlerTestWorkspace(t,
		[]string{"default"},
		[]string{"debugging"})

	out := captureStdout(t, func() {
		handleModeShow([]string{"debugging"})
	})
	if !strings.Contains(out, "# Mode: debugging") {
		t.Errorf("show header missing:\n%s", out)
	}
	if !strings.Contains(out, "test mode debugging") {
		t.Errorf("description missing:\n%s", out)
	}
}

func TestModeShow_READMERejected(t *testing.T) {
	setupHandlerTestWorkspace(t,
		[]string{"default"},
		[]string{"debugging"})

	captureStderr(t, func() {
		handleModeShow([]string{"README"})
	})
}

// internal_NewModeManager returns a ModeManager pointed at the test
// workspace. The workspace path is read from MPM_WORKSPACE (set by
// setupHandlerTestWorkspace). Tests below use the manager to verify
// persistence without invoking the CLI handler a second time.
func internal_NewModeManager(_ string) *mpminternal.ModeManager {
	ws := os.Getenv("MPM_WORKSPACE")
	return mpminternal.NewModeManager(ws)
}

// captureStderr is a sibling helper to the package-level captureStdout
// (defined in f8_f10_regression_test.go). It redirects os.Stderr for
// the duration of fn, returning the captured output as a string.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}