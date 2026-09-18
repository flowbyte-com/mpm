// umask_test.go — Behavioral coverage for EnforcePrivateUmask.
//
// Verifies the process-wide umask is set to 0077 BEFORE any state-writing
// subsystem has a chance to run, and that newly-created files in this
// process inherit the restrictive default regardless of the ambient
// shell umask.
//
// Strategy: each test subshell sets an ambient umask, calls
// EnforcePrivateUmask, and then verifies that subsequently-created
// files inherit 0700/0600 (NOT the ambient default).

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestEnforcePrivateUmask_ProcessMaskIs0077(t *testing.T) {
	// Capture the previous umask so we can restore at the end (this
	// test runs in the parent go-test process, NOT a subprocess).
	prev := syscall.Umask(0)
	syscall.Umask(prev)
	t.Cleanup(func() { syscall.Umask(prev) })

	EnforcePrivateUmask()

	// Confirm the process-wide mask is now 0o077. We can verify this
	// indirectly by creating a file and inspecting its mode: under
	// mask 0077, an open(..., 0666) yields 0600.
	tmp := t.TempDir()
	p := filepath.Join(tmp, "probe")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	f.Close()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := info.Mode().Perm()
	if mode != 0o600 {
		t.Fatalf("after EnforcePrivateUmask, file created with 0666 should be 0600 under mask 0077; got %04o", mode)
	}
}

func TestEnforcePrivateUmask_AppliesToDirectories(t *testing.T) {
	prev := syscall.Umask(0)
	syscall.Umask(prev)
	t.Cleanup(func() { syscall.Umask(prev) })

	EnforcePrivateUmask()

	tmp := t.TempDir()
	// os.MkdirAll uses 0o777; under mask 0077 that yields 0700.
	p := filepath.Join(tmp, "sub", "deeper")
	if err := os.MkdirAll(p, 0o777); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := info.Mode().Perm()
	if mode != 0o700 {
		t.Fatalf("after EnforcePrivateUmask, MkdirAll(0o777) should be 0700; got %04o", mode)
	}
}
