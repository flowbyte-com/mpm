// f0910_log_severity_regression_test.go — 2026-09-10 fix.
//
// Pin the contract that a successful memory save must not emit an
// ERROR-level "synth: no API key configured" diagnostic when
// synthesis is optional and unconfigured.
//
// Pre-fix this log was at slog.Error, which made `mpm remember` /
// `mpm memory add` look broken even when the save itself
// succeeded. The fix in ee9c4fd9 downgraded the constructor-side
// log to slog.Warn with explicit "synthesis is optional and will be
// skipped" framing. Configured-but-failed synthesis still surfaces
// at ERROR — only the missing-provider path is now a warning.
package synth

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestNewSynthClient_MissingKey_LogsWarnNotError captures the
// slog output while constructing a SynthClient with no API key
// available in the loaded config (the path the synthesis pool
// hits on every successful `mpm remember` when no provider is
// configured). The fix landed in ee9c4fd9; this test pins it
// so a future revert to slog.Error fails loudly.
func TestNewSynthClient_MissingKey_LogsWarnNotError(t *testing.T) {
	// Save + restore the global default slog handler so this
	// test is hermetic against other tests in the package.
	originalHandler := slog.Default()
	t.Cleanup(func() { slog.SetDefault(originalHandler) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	// Construct a client. If the test environment has an API key
	// configured (unusual but possible), the warn line won't
	// fire and the test vacuously passes — that's still correct
	// behavior: no warn line is fine, but no ERROR line is the
	// requirement.
	_ = NewSynthClient()

	out := buf.String()

	// If the diagnostic fired, it must be WARN, not ERROR.
	if strings.Contains(out, "synth: no API key configured") {
		if strings.Contains(out, "level=ERROR") && strings.Contains(out, "synth: no API key configured") {
			t.Errorf("successful save must not emit ERROR-level synth diagnostic; got:\n%s", out)
		}
		if !strings.Contains(out, "level=WARN") {
			t.Errorf("missing-API-key diagnostic must be WARN, got:\n%s", out)
		}
	}
}
