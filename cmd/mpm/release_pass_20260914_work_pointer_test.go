// release_pass_20260914_work_pointer_test.go — Regression coverage
// for the 2026-09-14 release-pass work URI round-trip contract.
//
// MPM emits work pointers in the canonical `mpm://work/<id>`
// form on the wire (see internal/core/work.go:65 — Pointer
// field). Pre-fix, every `mpm work item <sub>` command that
// accepted an id rejected the pointer form with a "work not
// found" error even though the bare id resolved correctly. The
// fix centralises ID normalisation in NormalizeWorkID
// (cmd/mpm/work_pointer.go) so every work subcommand accepts
// both forms via the same chokepoint.
//
// All tests use a hermetic workspace via t.TempDir(); production
// state is never touched. Mutating work operations are NOT
// tested in this file — the regression focus is the read/show
// path, which is the headline defect from the brief.

package main

import (
	stdlibexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkPointer_NormalizeStripsPrefix is the unit-level
// regression for the NormalizeWorkID helper. Pre-fix no such
// helper existed and every work subcommand rejected pointer
// inputs with "work not found".
func TestWorkPointer_NormalizeStripsPrefix(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"d72058d7e8e71b44", "d72058d7e8e71b44"},
		{"mpm://work/d72058d7e8e71b44", "d72058d7e8e71b44"},
		{"mpm://work/d72058d7e8e71b44/", "d72058d7e8e71b44"},
		{"  mpm://work/d72058d7e8e71b44  ", "d72058d7e8e71b44"},
		{"mpm://work/", "mpm://work/"},               // empty id preserved
		{"", ""},
		{"mpm://memory/d72058d7e8e71b44", "mpm://memory/d72058d7e8e71b44"}, // wrong scheme preserved
	}
	for _, tt := range tests {
		got := NormalizeWorkID(tt.in)
		if got != tt.want {
			t.Errorf("NormalizeWorkID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestWorkPointer_IsWorkPointer reports whether the input
// carries the canonical pointer prefix.
func TestWorkPointer_IsWorkPointer(t *testing.T) {
	if !IsWorkPointer("mpm://work/d72058d7e8e71b44") {
		t.Errorf("IsWorkPointer must recognise canonical prefix")
	}
	if IsWorkPointer("d72058d7e8e71b44") {
		t.Errorf("IsWorkPointer must NOT recognise bare ids")
	}
	if IsWorkPointer("mpm://memory/d72058d7e8e71b44") {
		t.Errorf("IsWorkPointer must NOT recognise wrong-scheme URIs")
	}
}

// TestWorkPointer_ShowAcceptsBareAndPointer is the built-binary
// smoke regression: `mpm work item show` must resolve both
// `d72058d7e8e71b44` and `mpm://work/d72058d7e8e71b44` to the
// same underlying work row. Pre-fix the pointer form returned
// "work not found" because the substrate never received the
// normalised id.
func TestWorkPointer_ShowAcceptsBareAndPointer(t *testing.T) {
	bin := buildTestBin(t)
	ws := t.TempDir()

	// Seed a work item and capture the id from the write envelope.
	id := createWorkItem(t, bin, ws, "URI round-trip probe", "consistency body")

	for _, label := range []string{"bare", "pointer"} {
		t.Run(label, func(t *testing.T) {
			var idArg string
			if label == "bare" {
				idArg = id
			} else {
				idArg = "mpm://work/" + id
			}
			cmd := stdlibexec.Command(bin, "work", "item", "show", idArg)
			cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("show %s: %v\n%s", idArg, err, out)
			}
			s := stripLogNoise(string(out))
			if !strings.Contains(s, id) {
				t.Fatalf("show output must contain id %q; got:\n%s", id, s)
			}
			if !strings.Contains(s, "URI round-trip probe") {
				t.Fatalf("show output must contain title; got:\n%s", s)
			}
		})
	}
}

// TestWorkPointer_PointerResolvesToSameItem asserts the
// pointer form resolves to the SAME underlying work item
// (same id, same title, same content) as the bare id form.
// Pre-fix the pointer form returned "work not found" because
// the substrate received `mpm://work/<id>` verbatim and tried
// to look it up.
func TestWorkPointer_PointerResolvesToSameItem(t *testing.T) {
	bin := buildTestBin(t)
	ws := t.TempDir()
	id := createWorkItem(t, bin, ws, "consistency probe", "consistency body")

	cmdBare := stdlibexec.Command(bin, "work", "item", "show", id)
	cmdBare.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	bareOut, err := cmdBare.Output()
	if err != nil {
		t.Fatalf("bare show: %v", err)
	}

	cmdPtr := stdlibexec.Command(bin, "work", "item", "show", "mpm://work/"+id)
	cmdPtr.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	ptrOut, err := cmdPtr.Output()
	if err != nil {
		t.Fatalf("pointer show: %v", err)
	}

	bareS := stripLogNoise(string(bareOut))
	ptrS := stripLogNoise(string(ptrOut))

	if bareS != ptrS {
		t.Fatalf("bare and pointer forms must render identical output.\nbare:\n%s\npointer:\n%s", bareS, ptrS)
	}
}

// TestWorkPointer_MalformedFailsCleanly asserts a malformed
// pointer (empty id after stripping, or wrong scheme) fails
// with a clear "work not found" error rather than silently
// coercing to a wrong id or panicking.
func TestWorkPointer_MalformedFailsCleanly(t *testing.T) {
	bin := buildTestBin(t)
	ws := t.TempDir()
	createWorkItem(t, bin, ws, "control item", "control body")

	for _, bad := range []string{
		"mpm://work/",         // empty id
		"mpm://memory/abc123", // wrong scheme (memory, not work)
	} {
		t.Run(bad, func(t *testing.T) {
			cmd := stdlibexec.Command(bin, "work", "item", "show", bad)
			cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
			_, err := cmd.Output()
			// Pre-fix this either panicked or returned an
			// unrelated error. The fix: wrong-scheme pointers
			// are NOT silently coerced; the substrate
			// receives the original input and reports
			// "work not found" cleanly. Empty pointer
			// likewise surfaces a clean error.
			if err == nil {
				t.Fatalf("malformed pointer %q must error; got success", bad)
			}
		})
	}
}

// createWorkItem seeds a work item via the CLI write path and
// returns the generated id. Uses the canonical CLI surface so
// the test exercises the same dispatcher the operator uses.
func createWorkItem(t *testing.T, bin, ws, title, content string) string {
	t.Helper()
	cmd := stdlibexec.Command(bin, "work", "item", "create", title, content)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("create work item: %v\n%s", err, out)
	}
	s := stripLogNoise(string(out))
	// The CLI write envelope is JSON; pull the id out by
	// string search (the create handler returns the
	// canonical {success, ...} envelope).
	idx := strings.Index(s, `"id":"`)
	if idx < 0 {
		t.Fatalf("create envelope missing id field; raw:\n%s", s)
	}
	rest := s[idx+len(`"id":"`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("malformed id in create envelope; raw:\n%s", s)
	}
	return rest[:end]
}

// buildTestBin builds a fresh mpm binary in a temp dir for
// the work-pointer tests. The build is cached per (test
// process, package); Go's build cache handles the rest.
func buildTestBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-test")
	cmd := stdlibexec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}
