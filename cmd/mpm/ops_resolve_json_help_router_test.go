// ops_resolve_json_help_router_test.go — the `--json --help` contract at
// the ROUTER, not at the handler (issue #21, Phase B, final review
// corrections).
//
// The gap this pins
//
// The handler honours `--json --help`: it prints one JSON envelope with
// the help inside it, and exits zero. But the handler is not what a
// shell invocation reaches first.
//
// Execute intercepts a help request BEFORE dispatching, and the
// interception renders the family's help page — plain text, to stdout.
// The interception is skipped only when the request matches an entry in
// subcommandHelpOwners, and that lookup is made against the token
// sequence between the family name and the trailing "help" token:
//
//	["ops", "resolve-contradictions", "--json", "help"]
//	         path = "ops resolve-contradictions --json"    (not an owner)
//
// So the --json flag pushed the path out of the owners map, the family
// page was rendered, and `mpm ops resolve-contradictions --json --help`
// printed PLAIN HELP TEXT on stdout — in a mode whose contract says
// stdout carries exactly one JSON document.
//
// A handler-level test cannot see this. The handler is correct; the
// dispatch in front of it is not. So these tests run the BINARY, the
// way an operator reaches the command.
//
// The human path
//
// The fix is scoped to JSON mode on purpose. `mpm ops
// resolve-contradictions --help` must keep printing the command's own
// page, and a human invocation that carries a flag in front of the help
// token must keep resolving to the family page exactly as it did. Both
// are pinned below, so the JSON fix cannot silently cost the human path
// anything.
//
// Every command runs against an isolated MPM_WORKSPACE + HOME with all
// model providers pointed at a closed port. Nothing touches the
// operator's real database.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runResolveCLI runs `mpm ops resolve-contradictions <args...>` in the
// isolated environment and returns stdout, stderr and the exit code
// SEPARATELY.
//
// The separation is the point. A JSON contract is a claim about stdout
// specifically, and CombinedOutput would hide a plain help page that
// arrived on stderr behind the document that arrived on stdout.
func runResolveCLI(t *testing.T, bin, workspace string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"ops", "resolve-contradictions"}, args...)...)
	cmd.Env = env
	cmd.Dir = workspace

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %v: %v\n%s", args, err, stdout.String())
		}
		code = exitErr.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// isolatedResolveWorkspace prepares an isolated workspace and home, and
// returns the binary, the workspace path, the home path and the
// environment.
func isolatedResolveWorkspace(t *testing.T) (string, string, string, []string) {
	t.Helper()
	bin := buildTestBinary(t)

	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")
	home := filepath.Join(tmp, "home")
	for _, d := range []string{workspace, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return bin, workspace, home, isolatedEnv(workspace, home)
}

// THE REGRESSION. `mpm ops resolve-contradictions --json --help` must
// print exactly one JSON document on stdout, and nothing else.
//
// Before the fix, Execute intercepted the request and printed the
// generic `mpm ops` family page to stdout — plain text, in JSON mode.
//
// The order of the two flags is irrelevant: the JSON mode is decided
// from the WHOLE argument list, so `--help --json` is the same request
// as `--json --help`. Both orders are pinned.
func TestOpsResolveContradictions_JSONHelpAtTheRouterIsOneJSONDocument(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}
	bin, workspace, _, env := isolatedResolveWorkspace(t)

	cases := []struct {
		name string
		args []string
	}{
		{"json then help", []string{"--json", "--help"}},
		{"json then h", []string{"--json", "-h"}},
		{"help then json", []string{"--help", "--json"}},
		{"h then json", []string{"-h", "--json"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runResolveCLI(t, bin, workspace, env, tc.args...)

			if code != 0 {
				t.Fatalf("a JSON help request must exit 0; got %d\nstdout: %s\nstderr: %s",
					code, clip(stdout, 400), clip(stderr, 400))
			}

			// The whole of stdout must be ONE JSON document. json.Unmarshal
			// rejects trailing content, so this catches a plain help page
			// printed after the document as well as one printed instead of
			// it.
			var env map[string]interface{}
			if err := json.Unmarshal([]byte(stdout), &env); err != nil {
				t.Fatalf("stdout is not exactly one JSON document: %v\nstdout: %s",
					err, clip(stdout, 600))
			}

			help, ok := env["help"].(string)
			if !ok || strings.TrimSpace(help) == "" {
				t.Fatalf("the envelope must carry the help text under %q; got %v", "help", env["help"])
			}
			if !strings.Contains(help, "--adjudicate=ID:REASON") {
				t.Errorf("the help inside the envelope is not the command's own help page:\n%s", clip(help, 400))
			}
			if env["error"] != nil {
				t.Errorf("a help request carries no error; got %v", env["error"])
			}

			// The plain family page must not be anywhere in stdout.
			//
			// The "one document" property is already proved by the
			// Unmarshal above, which rejects trailing content: a plain
			// help page printed after the document would fail it, and a
			// plain help page printed INSTEAD of it would fail the
			// unmarshal outright. What is checked here is the specific
			// page that used to be printed instead.
			if strings.Contains(stdout, familyPageMarker) {
				t.Errorf("stdout still contains the generic `mpm ops` family page:\n%s", clip(stdout, 600))
			}
		})
	}
}

// The human path is unchanged: the command's own page, on stdout, exit
// zero.
//
// The JSON fix must not have cost the human path its page — that page
// documents --adjudicate=ID:REASON, the flag that decides whether a row
// may penalise a memory at all.
func TestOpsResolveContradictions_HumanHelpAtTheRouterIsUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}
	bin, workspace, _, env := isolatedResolveWorkspace(t)

	stdout, _, code := runResolveCLI(t, bin, workspace, env, "--help")

	if code != 0 {
		t.Fatalf("a help request must exit 0; got %d", code)
	}
	if !strings.Contains(stdout, commandPageMarker) {
		t.Errorf("the human help path must still print the command's own page (%q absent):\n%s",
			commandPageMarker, clip(stdout, 600))
	}
	if strings.Contains(stdout, familyPageMarker) {
		t.Errorf("the human help path must not render the family page:\n%s", clip(stdout, 600))
	}
}

// A human invocation that carries a flag in front of the help token
// still resolves to the family page.
//
// This is the "retain existing non-JSON help behaviour" half of the
// contract, pinned so the JSON fix cannot quietly widen into the human
// path. The path lookup that made `--json --help` reach the handler
// deliberately drops the flags ONLY in JSON mode.
func TestOpsResolveContradictions_HumanHelpWithAFlagStillRendersTheFamilyPage(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}
	bin, workspace, _, env := isolatedResolveWorkspace(t)

	stdout, _, code := runResolveCLI(t, bin, workspace, env, "--propose", "--help")

	if code != 0 {
		t.Fatalf("a help request must exit 0; got %d", code)
	}
	if !strings.Contains(stdout, familyPageMarker) {
		t.Errorf("a non-JSON help request with a flag in front must still render the family page "+
			"(%q absent):\n%s", familyPageMarker, clip(stdout, 600))
	}
}

// The JSON-mode help request must be INERT: it opens no database and
// writes no queue row.
//
// The handler's help branch returns before getDBConcrete(). The routing
// change moved the request from the interception to the handler, so the
// inertness that the interception had is now the handler's to prove.
func TestOpsResolveContradictions_JSONHelpAtTheRouterIsInert(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}
	bin, workspace, homeDir, env := isolatedResolveWorkspace(t)

	// Seed first, so the digests below are taken against a settled
	// workspace rather than one the first command is still initialising.
	seedCanaries(t, bin, workspace, homeDir)
	dbPath := findDB(t, workspace)
	baseDB := digestDB(t, dbPath)
	baseFS := digestFS(t, workspace)

	stdout, _, code := runResolveCLI(t, bin, workspace, env, "--json", "--help")
	if code != 0 {
		t.Fatalf("a JSON help request must exit 0; got %d", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Fatalf("stdout must be the JSON document:\n%s", clip(stdout, 400))
	}

	if got := digestDB(t, dbPath); got != baseDB {
		t.Error("the JSON help request MUTATED the database; the help branch must return before getDBConcrete()")
	}
	if got := digestFS(t, workspace); got != baseFS {
		t.Error("the JSON help request MUTATED the workspace; the help branch must return before getDBConcrete()")
	}
}
