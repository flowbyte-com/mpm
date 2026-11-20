// handlers_reinforce_weaken_help_test.go — pins the --help short-circuit
// for handleReinforce / handleWeaken. The other CLI handlers in this
// family (handleMemoryAdd, handleRefAdd, handleWorkItem) already short-
// circuit on `-h` / `--help` / `help` because the router-level parseFlags
// rewrites --help to the literal token "help" before the handler sees
// the args. handleReinforce/handleWeaken missed the same short-circuit
// and fell through with "no row with id help (not found, deleted, or
// expired)" — operator sees a DB-error-styled message instead of help.
//
// Fix is the same three-token short-circuit already used by handleMemoryAdd
// (handlers_memory.go:138-142). The test asserts the help text is reached
// by routing through usererror.Usage (which writes to the usererror
// writer), without touching the DB.
package main

import (
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/usererror"
)

func TestHandleReinforce_HelpShortCircuit(t *testing.T) {
	cases := []string{"-h", "--help", "help"}
	for _, token := range cases {
		t.Run(token, func(t *testing.T) {
			var out string
			usererror.SetWriter(stringWriter(&out))
			defer usererror.SetWriter(nil)

			code := handleReinforce([]string{"reinforce", token})
			if code == 0 {
				t.Fatalf("handleReinforce(%q) returned 0 — must signal help, not succeed", token)
			}
			// Help text must reach the usererror writer (not stderr).
			// `mpm reinforce <id> [delta]` is the canonical Usage line.
			if !strings.Contains(out, "mpm reinforce") {
				t.Errorf("handleReinforce(%q) missing Usage line in output: %q", token, out)
			}
		})
	}
}

func TestHandleWeaken_HelpShortCircuit(t *testing.T) {
	cases := []string{"-h", "--help", "help"}
	for _, token := range cases {
		t.Run(token, func(t *testing.T) {
			var out string
			usererror.SetWriter(stringWriter(&out))
			defer usererror.SetWriter(nil)

			code := handleWeaken([]string{"weaken", token})
			if code == 0 {
				t.Fatalf("handleWeaken(%q) returned 0 — must signal help, not succeed", token)
			}
			if !strings.Contains(out, "mpm weaken") {
				t.Errorf("handleWeaken(%q) missing Usage line in output: %q", token, out)
			}
		})
	}
}

// stringWriter is a minimal io.Writer for the usererror.SetWriter slot.
type stringWriterHandle struct {
	buf *string
}

func (w *stringWriterHandle) Write(p []byte) (int, error) {
	*w.buf += string(p)
	return len(p), nil
}

func stringWriter(buf *string) *stringWriterHandle { return &stringWriterHandle{buf: buf} }