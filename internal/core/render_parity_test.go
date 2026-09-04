// render_parity_test.go — process-level CI gate for the agent
// installation render script.
//
// The Python `agent_installation/scripts/render_managed_blocks.py`
// extracts the host-neutral canonical managed block from
// MPM_AGENT_INTEGRATION_SNIPPETS.md and renders it for each
// persistent-file host (Claude, OpenCode, Pi, Hermes). It then
// compares the rendered output against the checked-in adapter
// template snippets and the per-host copy/paste examples inside the
// canonical source. Drift here means a future edit to the canonical
// block or to the renderer broke byte-for-byte parity.
//
// This Go test wires the Python `--check` into the canonical
// validation path (`make test` / `make test-race`). Without it, a
// render drift would only be caught by manually running the Python
// script — easy to forget on routine Go edits. With it, render drift
// fails the same CI gate as any other regression.
//
// The Python test file
// (`agent_installation/tests/test_render_managed_blocks.py`) remains
// the authoritative correctness surface for the renderer's semantic
// behaviour; this Go test only asserts that the process-level check
// script exits 0 (the canonical-source / adapter-snippet /
// copy-paste-example parity contract).
package internal

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRender_ManagedBlocksParity(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller must resolve this test file")
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	require.NoError(t, err, "computing repo root from this test file location")

	cmd := exec.Command(
		"python3",
		filepath.Join(root, "agent_installation", "scripts", "render_managed_blocks.py"),
		"--check",
	)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	require.NoError(
		t, cmd.Run(),
		"render_managed_blocks.py --check failed (drift in canonical "+
			"managed block):\nstdout: %s\nstderr: %s",
		stdout.String(), stderr.String(),
	)
}
