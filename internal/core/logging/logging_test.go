package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestSetupWithWriter_DebugLevel(t *testing.T) {
	t.Setenv("MPM_LOG", "debug")
	var buf bytes.Buffer
	SetupWithWriter(&buf)
	slog.Debug("debug-payload", "k", "v")
	if !strings.Contains(buf.String(), "debug-payload") {
		t.Fatalf("expected debug record, got: %q", buf.String())
	}
}

func TestSetupWithWriter_InfoDefault(t *testing.T) {
	t.Setenv("MPM_LOG", "")
	var buf bytes.Buffer
	SetupWithWriter(&buf)
	slog.Debug("hidden")
	slog.Info("visible", "k", "v")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Fatalf("debug should be hidden at info level, got: %q", out)
	}
	if !strings.Contains(out, "visible") {
		t.Fatalf("expected info record, got: %q", out)
	}
}

func TestSetupWithWriter_JSONFormat(t *testing.T) {
	t.Setenv("MPM_LOG_FORMAT", "json")
	var buf bytes.Buffer
	SetupWithWriter(&buf)
	slog.Info("test", "k", "v")
	out := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(out, "{") || !strings.HasSuffix(out, "}") {
		t.Fatalf("expected JSON object, got: %q", out)
	}
	if !strings.Contains(out, `"msg":"test"`) {
		t.Fatalf("expected msg field in JSON, got: %q", out)
	}
}

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"trace", slog.LevelDebug},
		{"garbage", slog.LevelInfo},
	}
	for _, c := range cases {
		t.Setenv("MPM_LOG", c.in)
		if got := parseLevel(c.in); got != c.want {
			t.Errorf("parseLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}