package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile is a small helper for fixtures.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestValidateMode_RejectsEmptyNameAndTitle(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "empty.md")
	writeFile(t, path, "---\nversion: 1\n---\nbody\n")

	_, err := parseModeFile(path)
	if err == nil {
		t.Fatalf("expected error for mode with no name and no title")
	}
	if !strings.Contains(err.Error(), "no name and no title") {
		t.Errorf("expected 'no name and no title' in error, got: %v", err)
	}
}

func TestValidateMode_AcceptsNameOnly(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "named.md")
	writeFile(t, path, "---\nname: 808\n---\nbody\n")

	m, err := parseModeFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Name != "808" {
		t.Errorf("Name = %q, want 808", m.Name)
	}
}

func TestValidateMode_AcceptsTitleOnly(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "titled.md")
	writeFile(t, path, "---\ntitle: The 808 Mode\n---\nbody\n")

	m, err := parseModeFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Title != "The 808 Mode" {
		t.Errorf("Title = %q, want The 808 Mode", m.Title)
	}
}

func TestValidateMode_RejectsInvalidName(t *testing.T) {
	tmp := t.TempDir()
	cases := []string{
		"Bad Name",   // space
		"bad/name",   // slash
		"bad:name",   // colon
		"UPPER",      // uppercase
		"",           // empty (caught by name+title check)
	}
	for _, name := range cases {
		path := filepath.Join(tmp, "x.md")
		// We use title-only to focus the test on the name validator; for
		// the empty name case we also need to skip the title to trigger
		// the name validator.
		content := "---\ntitle: t\n"
		if name != "" {
			content = "---\nname: " + name + "\n"
		}
		writeFile(t, path, content)
		_, err := parseModeFile(path)
		// Empty name + title present is a separate test (above). All other
		// cases must hit the name validator.
		if name == "" {
			if err == nil || !strings.Contains(err.Error(), "no name and no title") {
				t.Errorf("name=%q: expected name+title error, got: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("name=%q: expected validation error, got nil", name)
		}
	}
}

func TestValidateMode_RejectsInvalidVersion(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "ver.md")
	writeFile(t, path, "---\nname: x\nversion: v1\n---\nbody\n")
	_, err := parseModeFile(path)
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("expected version error, got: %v", err)
	}
}

func TestValidatePersona_RejectsEmptyNameAndTitle(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "empty.md")
	writeFile(t, path, "---\nversion: 1\n---\nbody\n")
	_, err := parsePersonaFile(path)
	if err == nil {
		t.Fatalf("expected error for persona with no name and no title")
	}
}

func TestValidatePersona_AcceptsNameOnly(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "named.md")
	writeFile(t, path, "---\nname: openclaw-808\n---\nbody\n")
	p, err := parsePersonaFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Name != "openclaw-808" {
		t.Errorf("Name = %q, want openclaw-808", p.Name)
	}
}

func TestValidatePersona_RejectsInvalidName(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "bad.md")
	writeFile(t, path, "---\nname: Bad Name\n---\nbody\n")
	_, err := parsePersonaFile(path)
	if err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Fatalf("expected name validation error, got: %v", err)
	}
}