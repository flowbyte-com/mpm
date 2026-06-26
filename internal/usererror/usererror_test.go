package usererror

import (
	"bytes"
	"strings"
	"testing"
)

func TestError_PrefixAndNewline(t *testing.T) {
	var buf bytes.Buffer
	SetWriter(&buf)
	defer SetWriter(nil)
	code := Error("ingest failed: %v", "boom")
	if code != 1 {
		t.Errorf("expected exit 1, got %d", code)
	}
	if got := buf.String(); !strings.HasPrefix(got, "❌ ingest failed: boom\n") {
		t.Errorf("unexpected output: %q", got)
	}
}

func TestWarn_Prefix(t *testing.T) {
	var buf bytes.Buffer
	SetWriter(&buf)
	defer SetWriter(nil)
	Warn("missing config")
	if got := buf.String(); !strings.HasPrefix(got, "[!] missing config\n") {
		t.Errorf("unexpected output: %q", got)
	}
}

func TestNotice_Prefix(t *testing.T) {
	var buf bytes.Buffer
	SetWriter(&buf)
	defer SetWriter(nil)
	Notice("backfilled 12 items")
	if got := buf.String(); !strings.HasPrefix(got, "✓ backfilled 12 items\n") {
		t.Errorf("unexpected output: %q", got)
	}
}

func TestUsage_Prefix(t *testing.T) {
	var buf bytes.Buffer
	SetWriter(&buf)
	defer SetWriter(nil)
	Usage("mpm %s <file>", "ingest")
	if got := buf.String(); !strings.HasPrefix(got, "Usage: mpm ingest <file>\n") {
		t.Errorf("unexpected output: %q", got)
	}
}

func TestError_AppendsNewlineIfMissing(t *testing.T) {
	var buf bytes.Buffer
	SetWriter(&buf)
	defer SetWriter(nil)
	Error("no trailing newline")
	if got := buf.String(); !strings.HasSuffix(got, "\n") {
		t.Errorf("expected trailing newline, got: %q", got)
	}
}

func TestError_DoesNotDoublePrefix(t *testing.T) {
	var buf bytes.Buffer
	SetWriter(&buf)
	defer SetWriter(nil)
	// Convention: the message should not already include the ❌ prefix.
	// Helper adds it. The test confirms the message you pass is appended
	// after the prefix without modification.
	Error("plain text, no emoji")
	got := buf.String()
	if strings.Count(got, "❌") != 1 {
		t.Errorf("expected exactly one ❌, got %q", got)
	}
}