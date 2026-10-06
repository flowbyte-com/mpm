package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemember_StoresExactContent is the D1 regression test.
//
// Bug: router.go passed args[1:] to handleRemember, which delegates to
// handleAdd — and handleAdd strips args[0] again. Net effect:
//   - `mpm remember hello`          → "content required" error
//   - `mpm remember one two three`  → stored "two three" (silent data loss)
//
// Invariant: `mpm remember <content>` must persist exactly the supplied
// content, verified against PERSISTED STATE (via mpm_memory query), not
// merely command success.
func TestRemember_StoresExactContent(t *testing.T) {
	binPath := requireBuiltCLI(t)

	cases := []struct {
		name string
		args []string // arguments AFTER the verb "remember"
		want string   // exact content expected in storage
	}{
		{
			name: "single word",
			args: []string{"hello"},
			want: "hello",
		},
		{
			name: "multiple words",
			args: []string{"FirstWord", "second", "third"},
			want: "FirstWord second third",
		},
		{
			name: "punctuation",
			args: []string{"Warning:", "don't", "forget", "--flags;", "ok?"},
			want: "Warning: don't forget --flags; ok?",
		},
		{
			name: "quoted multi-word content",
			// Shell-level quoting collapses to ONE argv element; handler must
			// store it verbatim.
			args: []string{"Grafana dashboard lives at port 3001"},
			want: "Grafana dashboard lives at port 3001",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			workspace := filepath.Join(tmpDir, "workspace")

			run := func(args ...string) (string, int) {
				cmd := exec.Command(binPath, args...)
				cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+workspace)
				out, err := cmd.CombinedOutput()
				code := 0
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else if err != nil {
					t.Fatalf("run %v: %v", args, err)
				}
				return string(out), code
			}

			argv := append([]string{"remember"}, tc.args...)
			out, code := run(argv...)
			if code != 0 {
				t.Fatalf("mpm remember %v exited %d\noutput: %s", tc.args, code, out)
			}
			if !strings.Contains(out, "Added memory") {
				t.Fatalf("unexpected output: %s", out)
			}

			// Verify PERSISTED content through the product query surface,
			// matching on a distinctive tail token so a silently-truncated
			// first word cannot satisfy the assertion.
			query := "query the exact persisted text"
			switch tc.name {
			case "single word":
				query = "hello"
			case "multiple words":
				query = "third"
			case "punctuation":
				query = "forget"
			case "quoted multi-word content":
				query = "Grafana"
			}
			qout, qcode := run("call", "mpm_memory", "--payload",
				`{"action":"query","params":{"query":"`+query+`","limit":5}}`)
			if qcode != 0 {
				t.Fatalf("query exited %d\noutput: %s", qcode, qout)
			}
			if !strings.Contains(qout, tc.want) {
				t.Errorf("persisted content mismatch:\n want: %q\n query output: %s", tc.want, qout)
			}
		})
	}
}

// TestAdd_StoresExactContent guards the sibling command: the D1 fix must not
// change `mpm add` semantics (add takes full args including the verb).
func TestAdd_StoresExactContent(t *testing.T) {
	binPath := requireBuiltCLI(t)

	tmpDir := t.TempDir()
	workspace := filepath.Join(tmpDir, "workspace")
	env := append(os.Environ(), "MPM_WORKSPACE="+workspace)

	cmd := exec.Command(binPath, "add", "alpha", "beta", "gamma")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mpm add: %v\noutput: %s", err, out)
	}

	cmd = exec.Command(binPath, "call", "mpm_memory", "--payload",
		`{"action":"query","params":{"query":"gamma","limit":5}}`)
	cmd.Env = env
	qout, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("query: %v\noutput: %s", err, qout)
	}
	if !strings.Contains(string(qout), "alpha beta gamma") {
		t.Errorf("add stored %q, want %q", strings.TrimSpace(string(qout)), "alpha beta gamma")
	}
}
