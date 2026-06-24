package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParsePayload_FromPayloadFlag covers the inline --payload path,
// which is the only path that previously worked reliably but still
// trips up on apostrophes due to shell escaping.
func TestParsePayload_FromPayloadFlag(t *testing.T) {
	got, err := parsePayload([]string{"--payload", `{"fact":"hello world","weight":0.7}`})
	if err != nil {
		t.Fatalf("parsePayload: %v", err)
	}
	if got["fact"] != "hello world" {
		t.Errorf("fact = %v, want %q", got["fact"], "hello world")
	}
	if got["weight"] != 0.7 {
		t.Errorf("weight = %v, want 0.7", got["weight"])
	}
}

// TestParsePayload_FromPayloadFile covers --payload-file, the new path
// that lets callers avoid shell escaping entirely.
func TestParsePayload_FromPayloadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.json")
	// Intentionally include characters that would break inline --payload.
	contents := `{"fact":"don't stop — it's fine! $100 (USD)","tags":["test"]}`
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := parsePayload([]string{"--payload-file", path})
	if err != nil {
		t.Fatalf("parsePayload: %v", err)
	}
	want := "don't stop — it's fine! $100 (USD)"
	if got["fact"] != want {
		t.Errorf("fact = %v, want %q", got["fact"], want)
	}
}

// TestParsePayload_FromPayloadFile_BOM covers UTF-8 BOM handling
// (some editors prepend a BOM, which would break json.Unmarshal).
func TestParsePayload_FromPayloadFile_BOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.json")
	bom := []byte{0xEF, 0xBB, 0xBF}
	body := []byte(`{"fact":"bom-test"}`)
	if err := os.WriteFile(path, append(bom, body...), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := parsePayload([]string{"--payload-file", path})
	if err != nil {
		t.Fatalf("parsePayload: %v", err)
	}
	if got["fact"] != "bom-test" {
		t.Errorf("fact = %v, want bom-test", got["fact"])
	}
}

// TestParsePayload_PayloadFlagTakesPrecedence ensures --payload is read
// when it is the only payload source present.
func TestParsePayload_PayloadFlagTakesPrecedence(t *testing.T) {
	got, err := parsePayload([]string{"--payload", `{"fact":"flag-wins"}`})
	if err != nil {
		t.Fatalf("parsePayload: %v", err)
	}
	if got["fact"] != "flag-wins" {
		t.Errorf("fact = %v, want flag-wins", got["fact"])
	}
}

// TestParsePayload_NoArgsNoStdin ensures an empty result when neither
// --payload nor stdin data is present. Required for tools like
// read_wake_context that take no input.
func TestParsePayload_NoArgsNoStdin(t *testing.T) {
	got, err := parsePayload([]string{})
	if err != nil {
		t.Fatalf("parsePayload: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

// TestParsePayload_LastFlagWins documents the priority order: when both
// --payload and --payload-file appear in argv, the LAST one wins (standard
// CLI convention — git, kubectl, etc.). Stdin is the fallback when no flag
// is present at all.
func TestParsePayload_LastFlagWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(path, []byte(`{"fact":"from-file"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// --payload comes after --payload-file, so it wins.
	got, err := parsePayload([]string{
		"--payload-file", path,
		"--payload", `{"fact":"from-flag"}`,
	})
	if err != nil {
		t.Fatalf("parsePayload: %v", err)
	}
	if got["fact"] != "from-flag" {
		t.Errorf("fact = %v, want from-flag (last --payload should win)", got["fact"])
	}
	// Reverse order: --payload-file comes after --payload, so it wins.
	got, err = parsePayload([]string{
		"--payload", `{"fact":"from-flag"}`,
		"--payload-file", path,
	})
	if err != nil {
		t.Fatalf("parsePayload: %v", err)
	}
	if got["fact"] != "from-file" {
		t.Errorf("fact = %v, want from-file (last --payload-file should win)", got["fact"])
	}
}

// TestParsePayload_PayloadFile_NotFound covers the missing-file error path.
func TestParsePayload_PayloadFile_NotFound(t *testing.T) {
	_, err := parsePayload([]string{"--payload-file", "/nonexistent/payload.json"})
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// TestParsePayload_PayloadFile_InvalidJSON covers the parse-error path.
func TestParsePayload_PayloadFile_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(path, []byte(`{not-json`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := parsePayload([]string{"--payload-file", path})
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}