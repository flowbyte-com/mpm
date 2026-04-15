package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadIdentity(t *testing.T) {
	// Create temp dir with IDENTITY.md
	dir := t.TempDir()
	identity := `# TestBot v1.0
Type: test assistant
Core traits: precise, analytical`
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(identity), 0644); err != nil {
		t.Fatal(err)
	}

	id, err := LoadIdentity(filepath.Join(dir, "IDENTITY.md"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if id == nil {
		t.Fatal("expected non-nil Identity")
	}

	if id.Name != "TestBot" {
		t.Errorf("expected Name 'TestBot', got '%s'", id.Name)
	}

	if id.Version != "1.0" {
		t.Errorf("expected Version '1.0', got '%s'", id.Version)
	}

	if id.Type != "test assistant" {
		t.Errorf("expected Type 'test assistant', got '%s'", id.Type)
	}

	if id.Traits != "precise, analytical" {
		t.Errorf("expected Traits 'precise, analytical', got '%s'", id.Traits)
	}
}

func TestLoadIdentityFromFile(t *testing.T) {
	// Create a temp directory with a valid IDENTITY.md
	dir := t.TempDir()
	identity := `# MiniBot v2.0
Type: personal assistant
Domain: productivity
Core traits: helpful, concise
Boundaries: no personal advice`
	if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(identity), 0644); err != nil {
		t.Fatal(err)
	}

	id, err := LoadIdentity(filepath.Join(dir, "IDENTITY.md"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if id == nil {
		t.Fatal("expected non-nil Identity")
	}

	if id.Name != "MiniBot" {
		t.Errorf("expected Name 'MiniBot', got '%s'", id.Name)
	}

	if id.Version != "2.0" {
		t.Errorf("expected Version '2.0', got '%s'", id.Version)
	}

	if id.Type != "personal assistant" {
		t.Errorf("expected Type 'personal assistant', got '%s'", id.Type)
	}

	if id.Domain != "productivity" {
		t.Errorf("expected Domain 'productivity', got '%s'", id.Domain)
	}

	if id.Traits != "helpful, concise" {
		t.Errorf("expected Traits 'helpful, concise', got '%s'", id.Traits)
	}

	if id.Boundaries != "no personal advice" {
		t.Errorf("expected Boundaries 'no personal advice', got '%s'", id.Boundaries)
	}
}

func TestLoadIdentityFileNotFound(t *testing.T) {
	_, err := LoadIdentity("/nonexistent/path/IDENTITY.md")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func containsField(text, field string) bool {
	return strings.Contains(text, field)
}
