// config_malformed_test.go — a malformed mpm_config.json must be a
// CONTROLLED VALIDATION ERROR, not a panic.
//
// THE DEFECT THIS PINS
//
// config.LoadConfig() returns (nil, err) when mpm_config.json does not
// unmarshal. internal/core/memory.go's DefaultMemoryPaths() discarded
// that error with `_` and dereferenced the nil config on the next line:
//
//	cfg, _ := config.LoadConfig()
//	memoryPath := config.ResolveEnvPath(cfg.MemoryDir)   // SIGSEGV
//
// So a single typo in a component binding — `components` maps a
// component name to a PROFILE NAME, so its values must be strings, not
// booleans — turned `mpm memory save` into a SEGMENTATION FAULT with a
// stack trace, and every other command that consulted the configuration
// with it.
//
// The fix has two halves, and this file pins both:
//
//  1. config.ValidateConfig() reports the problem as an error that
//     NAMES THE FILE and the reason.
//  2. DefaultMemoryPaths() resolves the canonical paths anyway, so a
//     caller that reaches it before the gate does not crash.
//
// The failure this test guards against is the silent one: a config error
// that is swallowed, or a panic that a stack trace has to explain.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withConfigFile writes content to <workspace>/mpm_config.json inside a
// disposable workspace and points MPM_WORKSPACE at it for the duration
// of the test.
func withConfigFile(t *testing.T, content string) string {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	path := filepath.Join(ws, "mpm_config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestValidateConfig_MalformedIsNamedError is the headline case: a
// malformed file is an error that names the file.
func TestValidateConfig_MalformedIsNamedError(t *testing.T) {
	path := withConfigFile(t, `{"components": {"memory": true}}`)

	err := ValidateConfig()
	if err == nil {
		t.Fatal("a malformed mpm_config.json must be an error, not nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, path) {
		t.Errorf("the error must name the offending file %q; got: %s", path, msg)
	}
	if !strings.Contains(msg, "not valid JSON") {
		t.Errorf("the error must say the file is not valid JSON; got: %s", msg)
	}
	// The message must tell the operator what to DO.
	if !strings.Contains(msg, "mpm config set") {
		t.Errorf("the error must point at the repair command; got: %s", msg)
	}
}

// TestValidateConfig_ValidAndMissingAreNotErrors pins the two states that
// must NOT be errors: a well-formed file, and no file at all.
func TestValidateConfig_ValidAndMissingAreNotErrors(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		withConfigFile(t, `{"profiles": {"default": {"provider": "x"}}}`)
		if err := ValidateConfig(); err != nil {
			t.Fatalf("a valid configuration must validate: %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		ws := t.TempDir()
		t.Setenv("MPM_WORKSPACE", ws)
		if err := ValidateConfig(); err != nil {
			t.Fatalf("a missing configuration is a normal state, not an error: %v", err)
		}
	})
}

// TestValidateConfig_UnreadableIsAnError pins the third state: a file
// that exists but cannot be read is an error that says so.
func TestValidateConfig_UnreadableIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a 0000-mode file")
	}
	path := withConfigFile(t, `{}`)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	err := ValidateConfig()
	if err == nil {
		t.Fatal("an unreadable configuration must be an error")
	}
	if !strings.Contains(err.Error(), "cannot read") {
		t.Errorf("the error must say the file cannot be read; got: %s", err)
	}
}

// TestLoadConfig_MalformedReturnsNilAndError pins the contract the
// nil-deref relied on getting wrong: LoadConfig reports the failure and
// returns no config.
func TestLoadConfig_MalformedReturnsNilAndError(t *testing.T) {
	withConfigFile(t, `{"components": {"memory": true}}`)
	cfg, err := LoadConfig()
	if err == nil {
		t.Fatal("a malformed configuration must produce an error")
	}
	if cfg != nil {
		t.Fatalf("a failed load must not return a config; got %#v", cfg)
	}
}
