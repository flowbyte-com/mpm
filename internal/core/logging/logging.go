// Package logging configures the package-level slog.Default() handler used
// throughout mpm.
//
// Policy:
//   - INFO level by default; set MPM_LOG=debug (or trace) for verbose output.
//   - Text format (human-readable) by default; set MPM_LOG_FORMAT=json for
//     structured JSON output suitable for log aggregators.
//   - All mpm binaries and library code log through slog.Default(), so calling
//     Setup() once at process start is the single switch.
//
// User-facing CLI errors (e.g. "Error: missing flag", "❌ Ingest failed") are
// NOT routed through slog — they go directly to stderr via fmt.Fprintf, since
// the user expects human-readable output, not log records. Internal warnings
// from background goroutines (audit, decay, synthesis, migration) DO use
// slog.Warn so they show up in operator dashboards and get leveled filtering.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Setup configures slog.Default() and returns the configured logger.
//
// Configuration via env vars (all optional):
//   - MPM_LOG=debug|info|warn|error (default: info)
//   - MPM_LOG_FORMAT=text|json      (default: text)
func Setup() *slog.Logger {
	return SetupWithWriter(os.Stderr)
}

// SetupWithWriter is Setup with a configurable output destination.
// Used by tests to redirect log output to a buffer.
func SetupWithWriter(w io.Writer) *slog.Logger {
	level := parseLevel(os.Getenv("MPM_LOG"))
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if strings.EqualFold(os.Getenv("MPM_LOG_FORMAT"), "json") {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

// parseLevel maps the MPM_LOG env var to a slog.Level. Unknown values
// fall back to Info with a one-shot warning.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "trace", "all":
		// slog has no LevelTrace; map to Debug.
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}