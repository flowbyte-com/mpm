package internal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSanitiseFTS5Tokens_Quoting pins the core FTS5 fix. Bare
// tokens caused FTS5 to interpret content words as column names
// and fail with "no such column: X". Wrapping each token in
// phrase quotes forces FTS5 to treat every token as a literal
// search term.
func TestSanitiseFTS5Tokens_Quoting(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "simple content",
			input:    "remember the milk",
			expected: []string{`"remember"`, `"the"`, `"milk"`},
		},
		{
			// The regression case: "07" was being interpreted as a
			// column name by FTS5. With quoting, it's a literal
			// search term.
			name:     "regression: \"07\" was treated as a column",
			input:    "07 verify pattern",
			expected: []string{`"07"`, `"verify"`, `"pattern"`},
		},
		{
			// Common cognitive-substrate tokens that look like
			// column names.
			name:     "common false-positive tokens",
			input:    "CHOICE verified closed hole",
			expected: []string{`"CHOICE"`, `"verified"`, `"closed"`, `"hole"`},
		},
		{
			// FTS5 operators are stripped from the output entirely.
			name:     "FTS5 operators stripped",
			input:    "running AND walking OR jumping",
			expected: []string{`"running"`, `"walking"`, `"jumping"`},
		},
		{
			// FTS5 special characters are stripped from word
			// boundaries. The close-paren at the end of "running)"
			// gets stripped, leaving "running".
			name:     "FTS5 special chars stripped",
			input:    "running) (walking) *star*",
			expected: []string{`"running"`, `"walking"`, `"star"`},
		},
		{
			// After stripping, an empty token doesn't appear.
			name:     "empty tokens dropped",
			input:    "real   word",
			expected: []string{`"real"`, `"word"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitiseFTS5Tokens(tc.input)
			if !equalStringSlicesForTest(got, tc.expected) {
				t.Errorf("sanitiseFTS5Tokens(%q):\n  got      %q\n  expected %q",
					tc.input, got, tc.expected)
			}
		})
	}

	// Cap at 100 tokens to bound query length.
	t.Run("100 tokens cap", func(t *testing.T) {
		got := sanitiseFTS5Tokens(repeatWordTokens(150))
		if len(got) != 100 {
			t.Errorf("expected 100 tokens (cap), got %d", len(got))
		}
	})
}

// TestSynthesisEnabled_DefaultTrue pins the kill switch default.
// A missing field in mpm_config.json (i.e. older config files
// written before this commit) must default to enabled, NOT
// disabled. The whole point of the kill switch is opt-out, not
// opt-in.
func TestSynthesisEnabled_DefaultTrue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	cfgFile := filepath.Join(dir, "mpm_config.json")
	if err := writeConfigFileForTest(cfgFile, `{
  "synth": {
    "model": "MiniMax-M3",
    "api_key": "test",
    "base_url": "http://localhost:11434/v1"
  }
}`); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if !synthesisEnabled() {
		t.Error("expected synthesisEnabled() == true when field is missing")
	}
}

// TestSynthesisEnabled_ExplicitTrue pins the explicit-true case.
// A config with synthesis_enabled=true must return true. (Tests
// that the field is *parsed* correctly, not just default-true.)
func TestSynthesisEnabled_ExplicitTrue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	cfgFile := filepath.Join(dir, "mpm_config.json")
	if err := writeConfigFileForTest(cfgFile, `{
  "synthesis_enabled": true,
  "synth": {
    "model": "MiniMax-M3",
    "api_key": "test",
    "base_url": "http://localhost:11434/v1"
  }
}`); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if !synthesisEnabled() {
		t.Error("expected synthesisEnabled() == true when field is explicitly true")
	}
}

// TestSynthesisEnabled_ExplicitFalse pins the kill switch.
// The whole point of this commit: when synthesis_enabled is false,
// AutoSynthesize returns immediately without any FTS5 query or
// LLM call.
func TestSynthesisEnabled_ExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	cfgFile := filepath.Join(dir, "mpm_config.json")
	if err := writeConfigFileForTest(cfgFile, `{
  "synthesis_enabled": false,
  "synth": {
    "model": "MiniMax-M3",
    "api_key": "test",
    "base_url": "http://localhost:11434/v1"
  }
}`); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if synthesisEnabled() {
		t.Error("expected synthesisEnabled() == false when kill switch is engaged")
	}
}

// TestSynthesisEnabled_NilConfig pins the safe-default path.
// If config fails to load (or is nil), synthesisEnabled returns
// true. This is the fail-safe: a broken config must not silently
// disable synthesis. Operationally, this means a corrupted
// mpm_config.json without a synthesis_enabled field should
// continue to fire synthesis rather than silently break it.
func TestSynthesisEnabled_NilConfig(t *testing.T) {
	// Point at a directory that doesn't exist. config.LoadConfig
	// returns an empty default config in that case — but the
	// critical assertion is that synthesisEnabled() returns true.
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	// No mpm_config.json in this directory. LoadConfig should
	// return &Config{} with all fields nil/zero. synthesisEnabled
	// must default to true.
	if !synthesisEnabled() {
		t.Error("expected synthesisEnabled() == true when config is empty/default")
	}
}

// Test helpers below — all renamed to avoid name collisions with
// other test files in the same package (mode_persona_validation_test.go,
// vector_index_test.go, etc. all define their own writeFile and itoa).

// writeConfigFileForTest writes a config file with mode 0600.
// Renamed from writeFile to avoid collision with mode_persona_validation_test.go.
func writeConfigFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0600)
}

// repeatWordTokens returns a string of "w0 w1 w2 ... wN-1" with the
// requested count. Used to test the 100-token cap.
func repeatWordTokens(n int) string {
	b := make([]byte, 0, n*4)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, []byte("w")...)
		b = append(b, []byte(intToStr10ForTest(i))...)
	}
	return string(b)
}

// intToStr10ForTest is a tiny int-to-string helper, renamed to avoid
// collision with vector_index_test.go's itoa.
func intToStr10ForTest(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// equalStringSlicesForTest is a tiny helper for content equality.
func equalStringSlicesForTest(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// _ = time and context — keep these imports alive across
// future edits where the test file may grow to need them.
var _ = time.Now
var _ = context.Background
