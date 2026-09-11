package internal

import "testing"

// TestIsDocumentationFile pins the README exclusion. The dangerous case
// is a README carrying valid-looking frontmatter — the loaders MUST
// reject it before parsing.
func TestIsDocumentationFile(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected bool
	}{
		{"README.md bare", "README.md", true},
		{"readme.md lower", "readme.md", true},
		{"ReadMe.md mixed", "ReadMe.md", true},
		{"README.MD upper ext", "README.MD", true},
		{"README.txt", "README.txt", true},
		{"README bare", "README", true},
		{"README full path", "/home/user/.mpm/persona/README.md", true},
		{"valid persona", "critic.md", false},
		{"valid mode", "debugging.md", false},
		{"empty string", "", false},
		{"non-md txt", "notes.txt", false},
		{"json", "config.json", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsDocumentationFile(tc.input)
			if got != tc.expected {
				t.Errorf("IsDocumentationFile(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

// TestIsDefinitionFile pins the .md + non-documentation contract.
// This is the function loaders use to decide whether to parse a file.
func TestIsDefinitionFile(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected bool
	}{
		{"valid persona", "critic.md", true},
		{"valid mode", "debugging.md", true},
		{"README.md", "README.md", false},
		{"readme.md lower", "readme.md", false},
		{"README.txt wrong ext", "README.txt", false},
		{"empty", "", false},
		{"json", "config.json", false},
		{"txt", "notes.txt", false},
		{"path-prefixed valid", "/home/user/.mpm/persona/critic.md", true},
		{"path-prefixed readme", "/home/user/.mpm/persona/README.md", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsDefinitionFile(tc.input)
			if got != tc.expected {
				t.Errorf("IsDefinitionFile(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

// TestIsDefinitionFile_NeverSelectsValidFrontmatterOnDocumentation
// is the explicit regression for the fragile case: a README carrying
// valid-looking frontmatter is still documentation, never a definition.
// IsDefinitionFile does not look at file content (it operates on the
// filename alone), but this test exists to document the invariant
// prominently. The full content-level regression is exercised in the
// loader tests where a README.md with `name: README` frontmatter is
// verified not to appear in `mpm persona list`.
func TestIsDefinitionFile_NeverSelectsValidFrontmatterOnDocumentation(t *testing.T) {
	// Filename alone is sufficient — frontmatter cannot override.
	if IsDefinitionFile("README.md") {
		t.Error("README.md must never be a selectable definition regardless of frontmatter")
	}
	if IsDefinitionFile("readme.md") {
		t.Error("readme.md (lowercase) must never be a selectable definition")
	}
	if IsDefinitionFile("README") {
		t.Error("README (no ext) must never be a selectable definition")
	}
}