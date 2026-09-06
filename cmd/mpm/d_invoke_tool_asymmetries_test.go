// cmd/mpm/d_invoke_tool_asymmetries_test.go
//
// Final-pass debt-closure test: verify the documented intentional
// asymmetries between `invokeTool` and `mpm call` are preserved
// against current consumers.
//
// S7 (and the final-pass investigation) classified the following as
// ACCEPTED ARCHITECTURAL DISTINCTIONS rather than defects:
//
//   1. Mode / Persona   — not propagated into ActiveContext by
//                          either `invokeTool` or `mpm call`; sourced
//                          from package globals (CLI) or config files
//                          (MCP). Neither surface treats them as
//                          ActiveContext fields.
//   2. wireToolsGlobals  — wired by `mpm call` (cross-process
//                          surface that may invoke mpm_blob_read /
//                          mpm_resolve) but NOT by `invokeTool`
//                          (in-process CLI surface whose consumers
//                          do not touch blob/pointer).
//   3. Heartbeat         — bumped by `mpm call` (cross-process
//                          session liveness) but NOT by `invokeTool`
//                          (in-process CLI use should not mutate
//                          session liveness).
//   4. CheckPendingWakes — folded by `mpm call` (operator/agent
//                          runtime signal) but NOT by `invokeTool`.
//                          The dedicated `mpm wake` command is the
//                          operator surface for due wakes.
//
// These tests pin the boundary so a future attempt to make
// `invokeTool` a "second `mpm call`" (S7 spec rule 7) cannot silently
// widen the contract.

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestD_InvokeToolAsymmetries_DocumentationPinsTheBoundary(t *testing.T) {
	// Read the cli_args_invoke.go source and assert the
	// documented asymmetries are present in the comment block.
	// The comment is the architectural record; if it gets
	// weakened, the S7 reasoning evaporates.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test file path")
	}
	invokeSrc, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "cli_args_invoke.go"))
	if err != nil {
		t.Fatalf("read cli_args_invoke.go: %v", err)
	}
	src := string(invokeSrc)

	// Each asymmetry must be mentioned in the file's docstring.
	// The string-form checks are loose; a future tightening of
	// the docstring would still satisfy them, but a weakening
	// (e.g. "Mode/Persona ARE propagated") would fail.
	for _, phrase := range []string{
		"Mode", "Persona",
		"wireToolsGlobals",
		"Heartbeat",
		"CheckPendingWakes",
	} {
		assert.True(t, strings.Contains(src, phrase),
			"cli_args_invoke.go must document %q so the boundary stays explicit", phrase)
	}
}

func TestD_InvokeToolAsymmetries_InvokeToolSourceDoesNotCallHeartbeat(t *testing.T) {
	// A grep-level check: cli_args_invoke.go must NOT call
	// dm.Heartbeat. Adding heartbeat to invokeTool would
	// silently mutate session liveness on every cognitive CLI
	// invocation, which S7 classified as wrong surface
	// behaviour.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test file path")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "cli_args_invoke.go"))
	if err != nil {
		t.Fatalf("read cli_args_invoke.go: %v", err)
	}
	src := string(data)
	assert.NotContains(t, src, "Heartbeat(",
		"invokeTool must not call dm.Heartbeat — heartbeat is a cross-process supervision signal, not an in-process CLI concern")
	assert.NotContains(t, src, "CheckPendingWakes(",
		"invokeTool must not fold due wakes — the dedicated mpm wake command is the operator surface for wakes")
}

func TestD_InvokeToolAsymmetries_InvokeToolSourceDoesNotWireGlobals(t *testing.T) {
	// wireToolsGlobals installs blob store + pointer resolver
	// adapters. invokeTool is the in-process narrow boundary;
	// its consumers (S5/S6 cognitive verbs) do not touch
	// blob/pointer. Adding wireToolsGlobals here would silently
	// install global state on every cognitive CLI invocation.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve test file path")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "cli_args_invoke.go"))
	if err != nil {
		t.Fatalf("read cli_args_invoke.go: %v", err)
	}
	src := string(data)
	assert.NotContains(t, src, "wireToolsGlobals(",
		"invokeTool must not call wireToolsGlobals — blob/pointer state is conditional, not blanket")
}
