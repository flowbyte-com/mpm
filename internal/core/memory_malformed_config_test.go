// memory_malformed_config_test.go — DefaultMemoryPaths must not panic on a
// malformed configuration.
//
// THE DEFECT THIS PINS
//
// DefaultMemoryPaths() used to do:
//
//	cfg, _ := config.LoadConfig()
//	memoryPath := config.ResolveEnvPath(cfg.MemoryDir)
//
// LoadConfig returns (nil, err) when mpm_config.json does not
// unmarshal, so a malformed configuration made this a NIL DEREFERENCE —
// a SIGSEGV with a stack trace, from `mpm memory save` and from every
// other caller of the memory store.
//
// The fix resolves the canonical paths anyway. That is the defence in
// depth BEHIND the startup gate (config.ValidateConfig), which is what
// actually reports the problem to the operator: a panic is a crash, and a
// crash cannot name the file that is wrong.
//
// This test is deliberately in the `internal` package (the package
// clause of internal/core itself), not the `config` package, because
// the panic site is here.

package internal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// TestDefaultMemoryPaths_MalformedConfigDoesNotPanic is the regression.
//
// A malformed config must produce canonical paths, not a crash.
func TestDefaultMemoryPaths_MalformedConfigDoesNotPanic(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	if err := os.WriteFile(filepath.Join(ws, "mpm_config.json"),
		[]byte(`{"components": {"memory": true}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// The whole point: this used to panic.
	paths := DefaultMemoryPaths()

	want := filepath.Join(ws, "src", "db", "mpm.db")
	if paths.SQLiteDBPath != want {
		t.Errorf("SQLiteDBPath = %q, want %q", paths.SQLiteDBPath, want)
	}
	if paths.MirrorFilePath != filepath.Join(ws, "src", "db", "mirror.jsonl") {
		t.Errorf("MirrorFilePath = %q", paths.MirrorFilePath)
	}
	// The cascade fallbacks must still be resolved, not left empty.
	if paths.MemoryPath == "" {
		t.Error("MemoryPath must fall back to the workspace's memory directory")
	}
	if paths.SessionPath == "" {
		t.Error("SessionPath must fall back to the OpenClaw sessions directory")
	}
}

// TestNewMemoryStore_MalformedConfigDoesNotPanic covers the constructor
// that the panic actually surfaced through.
func TestNewMemoryStore_MalformedConfigDoesNotPanic(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	if err := os.WriteFile(filepath.Join(ws, "mpm_config.json"),
		[]byte(`{"components": {"memory": true}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store := NewMemoryStore("")
	if store == nil {
		t.Fatal("NewMemoryStore must return a store, not nil")
	}
	if store.SQLiteDBPath == "" {
		t.Error("the store must carry the canonical database path")
	}
}

// TestDefaultMemoryPaths_ValidConfigUnchanged pins that the fix did not
// change the behaviour for a WELL-FORMED configuration: an explicit
// memory_dir is still honoured.
func TestDefaultMemoryPaths_ValidConfigUnchanged(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	cascade := t.TempDir()
	cfg := `{"memory_dir": "` + cascade + `"}`
	if err := os.WriteFile(filepath.Join(ws, "mpm_config.json"),
		[]byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	paths := DefaultMemoryPaths()
	if paths.MemoryPath != cascade {
		t.Errorf("MemoryPath = %q, want the configured %q", paths.MemoryPath, cascade)
	}
	// And the configuration itself must still validate.
	if err := config.ValidateConfig(); err != nil {
		t.Errorf("a valid configuration must validate: %v", err)
	}
}
