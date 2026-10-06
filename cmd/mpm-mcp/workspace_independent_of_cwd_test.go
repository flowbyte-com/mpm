// cmd/mpm-mcp/workspace_independent_of_cwd_test.go — W-008 regression.
//
// Audit claim: MCP server required CWD to be inside the workspace to
// function. Current HEAD is correct (mpmcli.ResolveWorkspace reads
// MPM_WORKSPACE first, falls back to "."), but the contract needs a
// regression test that pins it from the public boundary.
//
// We test `mpmcli.ResolveWorkspace` directly: it is the function that
// mpm-mcp calls at startup. Exercising it with a matrix of CWD values
// proves the workspace resolution is CWD-independent.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/flowbyte-com/mpm-core/mpmcli"
)

// TestResolveWorkspaceHonoursEnvVar pins the W-008 contract: when
// MPM_WORKSPACE is set, ResolveWorkspace returns it regardless of CWD.
func TestResolveWorkspaceHonoursEnvVar(t *testing.T) {
	ws := t.TempDir()

	cases := []struct {
		name string
		cwd  string // process CWD
		env  string // MPM_WORKSPACE
	}{
		{
			name: "env_set_cwd_equals_workspace",
			cwd:  ws,
			env:  ws,
		},
		{
			name: "env_set_cwd_is_parent",
			cwd:  filepath.Dir(ws),
			env:  ws,
		},
		{
			name: "env_set_cwd_is_tmp",
			cwd:  os.TempDir(),
			env:  ws,
		},
		{
			name: "env_set_cwd_is_unrelated",
			cwd:  string(filepath.Separator),
			env:  ws,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Swap CWD for the duration of the subtest.
			origCWD, err := os.Getwd()
			if err != nil {
				t.Fatalf("getwd: %v", err)
			}
			t.Cleanup(func() {
				if err := os.Chdir(origCWD); err != nil {
					t.Errorf("restore cwd: %v", err)
				}
			})

			if tc.cwd != "" {
				if err := os.Chdir(tc.cwd); err != nil {
					t.Fatalf("chdir %q: %v", tc.cwd, err)
				}
			}

			t.Setenv("MPM_WORKSPACE", tc.env)

			got := mpmcli.ResolveWorkspace()

			// Compare via filepath.Clean to absorb platform-specific
			// path quirks (symlinks, trailing slashes, etc.).
			gotClean := filepath.Clean(got)
			wantClean := filepath.Clean(tc.env)
			if gotClean != wantClean {
				t.Errorf("ResolveWorkspace()=%q (clean=%q), want clean %q",
					got, gotClean, wantClean)
			}
		})
	}
}

// TestResolveWorkspaceDefaultsToCanonicalRuntimeWhenUnset pins the fallback:
// with MPM_WORKSPACE unset, the workspace is the canonical user runtime, not
// the current directory.
//
// This test is named for the failure it guards. The MCP server resolving its
// workspace to "." when the variable is unset is what made it bootstrap-create
// mode/ and persona/ inside whatever directory it was launched from and open a
// database there — a ghost substrate distinct from the canonical one. The
// installed unit sets MPM_WORKSPACE explicitly, but an explicit launch by
// hand must not be able to reproduce that.
func TestResolveWorkspaceDefaultsToCanonicalRuntimeWhenUnset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MPM_WORKSPACE", "")

	want := filepath.Join(home, ".mpm")
	if got := mpmcli.ResolveWorkspace(); got != want {
		t.Errorf("ResolveWorkspace()=%q, want %q (MPM_WORKSPACE unset)", got, want)
	}
}
