package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupHandlerTestWorkspace builds an isolated MPM workspace under
// t.TempDir() with optional persona/mode .md entries. The returned
// workspace path is set as MPM_WORKSPACE for the duration of the test
// via t.Setenv.
//
// All tests using this helper exercise only files inside the temp dir —
// the real ~/.mpm is never touched.
func setupHandlerTestWorkspace(t *testing.T, personas []string, modes []string) string {
	t.Helper()
	root := t.TempDir()
	if len(personas) > 0 {
		dir := filepath.Join(root, "persona")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("mkdir persona: %v", err)
		}
		for _, name := range personas {
			path := filepath.Join(dir, name+".md")
			content := "---\nname: " + name + "\ndescription: test persona " + name + "\n---\n# " + name + " body\n"
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
	}
	if len(modes) > 0 {
		dir := filepath.Join(root, "mode")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("mkdir mode: %v", err)
		}
		for _, name := range modes {
			path := filepath.Join(dir, name+".md")
			content := "---\nname: " + name + "\ndescription: test mode " + name + "\n---\n# " + name + " body\n"
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
	}
	t.Setenv("MPM_WORKSPACE", root)
	return root
}

// TestPersonaList_FilesystemBacked_NoHardcodedVocabulary verifies that
// the persona list reflects only the actual files in persona/ and
// excludes README-style documentation files.
func TestPersonaList_FilesystemBacked_NoHardcodedVocabulary(t *testing.T) {
	setupHandlerTestWorkspace(t,
		[]string{"critic", "default", "forensic"},
		[]string{"default"})

	out := captureStdout(t, func() {
		handlePersonaList()
	})

	for _, name := range []string{"critic", "default", "forensic"} {
		if !strings.Contains(out, name) {
			t.Errorf("list missing %q in output:\n%s", name, out)
		}
	}
	if strings.Contains(out, "README") {
		t.Errorf("list should not surface README — output:\n%s", out)
	}
}

// TestPersonaList_READMEWithValidFrontmatter_StillExcluded is the
// explicit regression for the fragile case: a README carrying valid
// frontmatter must NEVER become a selectable persona.
func TestPersonaList_READMEWithValidFrontmatter_StillExcluded(t *testing.T) {
	root := setupHandlerTestWorkspace(t,
		[]string{"critic", "default"},
		[]string{"default"})
	// Drop a README carrying valid-looking frontmatter.
	readme := "---\nname: README\ndescription: malicious\n---\n# readme\n"
	if err := os.WriteFile(filepath.Join(root, "persona", "README.md"), []byte(readme), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	out := captureStdout(t, func() {
		handlePersonaList()
	})
	if strings.Contains(out, "README") {
		t.Errorf("README with valid frontmatter must not appear in list:\n%s", out)
	}
	if !strings.Contains(out, "critic") || !strings.Contains(out, "default") {
		t.Errorf("valid personas must still appear:\n%s", out)
	}
}

// TestPersonaShow_ReadsActualDefinition exercises the new mpm persona show
// subcommand — must read the actual .md file, not a hardcoded catalog.
func TestPersonaShow_ReadsActualDefinition(t *testing.T) {
	setupHandlerTestWorkspace(t,
		[]string{"critic"},
		[]string{"default"})

	out := captureStdout(t, func() {
		handlePersonaShow([]string{"critic"})
	})
	if !strings.Contains(out, "# Persona: critic") {
		t.Errorf("show header missing in output:\n%s", out)
	}
	if !strings.Contains(out, "test persona critic") {
		t.Errorf("description not surfaced from frontmatter:\n%s", out)
	}
	if !strings.Contains(out, "# critic body") {
		t.Errorf("body not surfaced:\n%s", out)
	}
}

func TestPersonaShow_UnknownName_ReturnsError(t *testing.T) {
	setupHandlerTestWorkspace(t, []string{"critic"}, []string{"default"})

	captureStderr(t, func() {
		handlePersonaShow([]string{"ghost"})
	})
}

func TestPersonaShow_READMERejected(t *testing.T) {
	setupHandlerTestWorkspace(t, []string{"critic"}, []string{"default"})

	captureStderr(t, func() {
		handlePersonaShow([]string{"README"})
	})
}

// TestPersonaSet_ActiveJSONStoresPointer exercises explicit selection
// end-to-end: persona set writes a pointer to active.json (not ""),
// preserving explicit vs absent distinction on read.
func TestPersonaSet_ActiveJSONStoresPointer(t *testing.T) {
	root := setupHandlerTestWorkspace(t, []string{"critic", "default"}, []string{"default"})

	out := captureStdout(t, func() {
		handlePersonaSet([]string{"critic"})
	})
	if !strings.Contains(out, "Persona set: critic") {
		t.Errorf("set output unexpected: %s", out)
	}

	// Read raw JSON and verify persona is present with value "critic"
	// (not absent — explicit selection).
	data, err := os.ReadFile(filepath.Join(root, "active.json"))
	if err != nil {
		t.Fatalf("read active.json: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, ok := raw["persona"]
	if !ok {
		t.Errorf("persona field absent from active.json after set — should be explicit")
	}
	if s, _ := p.(string); s != "critic" {
		t.Errorf("persona=%v, want \"critic\"", p)
	}
}

func TestPersonaSet_UnknownName_ReturnsError(t *testing.T) {
	setupHandlerTestWorkspace(t, []string{"critic", "default"}, []string{"default"})

	captureStderr(t, func() {
		handlePersonaSet([]string{"ghost"})
	})
}

// TestPersonaClear_WritesExplicitEmpty is the key invariant: clear
// must write "" with the field PRESENT (not absent) so the wake
// context can distinguish explicit-clear from bootstrap fallback.
func TestPersonaClear_WritesExplicitEmpty(t *testing.T) {
	root := setupHandlerTestWorkspace(t, []string{"critic", "default"}, []string{"default"})

	// Set then clear.
	captureStdout(t, func() {
		handlePersonaSet([]string{"critic"})
	})
	captureStdout(t, func() {
		handlePersonaClear()
	})

	data, err := os.ReadFile(filepath.Join(root, "active.json"))
	if err != nil {
		t.Fatalf("read active.json: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, ok := raw["persona"]
	if !ok {
		t.Errorf("persona field absent — clear should write \"\" with key present")
	}
	if s, _ := p.(string); s != "" {
		t.Errorf("persona=%v after clear, want \"\"", p)
	}
}

// The captureStdout / captureStderr helpers are defined in
// f8_f10_regression_test.go (shared across multiple test files in
// this package). They accept func() rather than func() int.