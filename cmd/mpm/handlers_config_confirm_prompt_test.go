// handlers_config_confirm_prompt_test.go — HAL-9001 issue #61.
//
// The wizard's final "Save?" prompt discarded the error from
// bufio.Reader.ReadString, so EOF (Ctrl-D) was indistinguishable from
// an empty line — and the prompt's default is YES.
//
// An operator who pressed Ctrl-D to abandon the run therefore got the
// configuration COMMITTED, and the onboarding report then recorded the
// configuration as saved. The abort gesture produced the opposite of
// the documented outcome ("Aborted." means the configuration is
// exactly as it was).
//
// What these tests pin:
//
//   - EOF is a cancellation, never a consent;
//   - a read error is a cancellation, never a consent;
//   - an ACTUAL empty line keeps the documented [Y/n] default;
//   - a cancellation writes no configuration.
//
// The wizard itself needs a terminal (it refuses to run without one),
// so the session-level test drives it through a pseudo-terminal rather
// than a pipe. Every endpoint it could reach is loopback.
package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/creack/pty"
	"github.com/flowbyte-com/mpm-core/config"
)

// TestConfirmPrompt_EOFIsCancellation pins the headline correction.
//
// bufio.Reader.ReadString returns ("", io.EOF) at EOF. Treating that
// as an empty line resolves the prompt's [Y/n] default to YES, which
// commits the configuration the operator was trying to abandon.
func TestConfirmPrompt_EOFIsCancellation(t *testing.T) {
	if confirmPrompt(bufio.NewReader(strings.NewReader("")), "Save?") {
		t.Error("confirmPrompt returned true on EOF — EOF is a cancellation, never a consent")
	}
}

// TestConfirmPrompt_EmptyLineKeepsTheDefaultYes pins that the fix
// does not change the documented default.
//
// An operator who presses ENTER has answered the question; the [Y/n]
// default applies. Only the ABSENCE of an answer is a cancellation.
func TestConfirmPrompt_EmptyLineKeepsTheDefaultYes(t *testing.T) {
	if !confirmPrompt(bufio.NewReader(strings.NewReader("\n")), "Save?") {
		t.Error("confirmPrompt returned false on an empty line — the [Y/n] default is Yes")
	}
}

// TestConfirmPrompt_ExplicitAnswers pins the ordinary answer paths.
func TestConfirmPrompt_ExplicitAnswers(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"  y  \n", true},
		{"n\n", false},
		{"no\n", false},
		{"N\n", false},
		{"maybe\n", false},
	}
	for _, tc := range cases {
		t.Run(strings.TrimSpace(tc.input), func(t *testing.T) {
			if got := confirmPrompt(bufio.NewReader(strings.NewReader(tc.input)), "Save?"); got != tc.want {
				t.Errorf("confirmPrompt(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestConfirmPrompt_ReadErrorIsCancellation pins that an unreadable
// stdin is not an answer.
//
// A broken pipe, a closed descriptor or any other read failure means
// the operator's intent was never obtained. It is not consent.
func TestConfirmPrompt_ReadErrorIsCancellation(t *testing.T) {
	rw := bufio.NewReader(iotest.ErrReader(errors.New("stdin broke")))
	if confirmPrompt(rw, "Save?") {
		t.Error("confirmPrompt returned true on a read error — a read error is a cancellation, never a consent")
	}
}

// TestConfirmPrompt_UnterminatedAnswerIsCancellation pins the rule for
// an answer that never received its newline.
//
// "y" followed by Ctrl-D is an aborted line, not a complete answer.
// The rule stays simple: a confirmation is a terminated line, or it is
// not a confirmation.
func TestConfirmPrompt_UnterminatedAnswerIsCancellation(t *testing.T) {
	if confirmPrompt(bufio.NewReader(strings.NewReader("y")), "Save?") {
		t.Error("confirmPrompt returned true for an unterminated answer — a confirmation must be a complete line")
	}
}

// wizardAnswers is the operator's script for the wizard, one line per
// prompt:
//
//	Provider            → Custom (the only public entry)
//	Protocol            → Ollama (no credential prompt)
//	Model               → left empty, so the wizard probes no endpoint
//	Base URL            → the protocol's default
//	Embedding           → leave unchanged
//	Specialised models  → no separate critic model
//
// The model is left empty on purpose: a named model makes the wizard
// probe the endpoint's capabilities, and a regression test must not
// depend on what answers a live daemon.
//
// There is NO line for a request timeout. The timeout is not an
// onboarding question (see the timeout policy in
// internal/core/config/timeout.go), so the wizard does not ask for one.
// This script is also the check: if the prompt came back, the answers
// would misalign from that point on and the session would write the
// wrong values — which the wizard's own regression tests would catch.
const wizardAnswers = "1\n3\n\n\n1\nn\n"

// runWizardInPTY drives the interactive wizard through a
// pseudo-terminal and returns its exit code and whether the wizard
// committed the configuration.
//
// finalInput is what the operator supplies at the final "Save?"
// prompt: "y\n" to approve, or the terminal's EOF character to abort.
func runWizardInPTY(t *testing.T, c *config.Config, finalInput string) (int, bool) {
	t.Helper()

	ptmx, ptytty, err := pty.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}

	origStdin := os.Stdin
	os.Stdin = ptytty
	restoreStdin := true
	t.Cleanup(func() {
		if restoreStdin {
			os.Stdin = origStdin
		}
		_ = ptmx.Close()
		_ = ptytty.Close()
	})

	if _, err := io.WriteString(ptmx, wizardAnswers+finalInput); err != nil {
		t.Fatalf("write the session to the pseudo-terminal: %v", err)
	}

	type result struct {
		code      int
		committed bool
	}
	done := make(chan result, 1)
	go func() {
		code, committed := handleConfigInteractive(c)
		done <- result{code: code, committed: committed}
	}()

	select {
	case res := <-done:
		return res.code, res.committed
	case <-time.After(30 * time.Second):
		// Close the master so the pending read fails and the wizard
		// goroutine can finish, and leave os.Stdin alone: restoring it
		// while the wizard is still reading it would be a data race.
		restoreStdin = false
		_ = ptmx.Close()
		t.Fatal("the wizard did not finish within 30s — it is probably waiting for an answer the script does not supply")
		return 1, false
	}
}

// TestWizard_EOFFinalConfirmationWritesNoConfiguration is the
// session-level regression test for issue #61.
//
// Ctrl-D at the final "Save?" prompt must leave the operator's
// configuration exactly as it was.
func TestWizard_EOFFinalConfirmationWritesNoConfiguration(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := &config.Config{}

	runWizardInPTY(t, c, "\x04") // Ctrl-D: the terminal's EOF character.

	if _, err := os.Stat(config.ConfigPath()); err == nil {
		t.Errorf("the wizard wrote %s after the operator aborted with EOF; an abort must leave the configuration exactly as it was",
			config.ConfigPath())
	}
}

// TestWizard_YesFinalConfirmationWritesTheConfiguration is the
// control for the test above.
//
// The same session, differing only in the final answer, MUST commit.
// Without this control, "no configuration was written" could be true
// for the wrong reason — a session that never reached the final
// prompt at all.
func TestWizard_YesFinalConfirmationWritesTheConfiguration(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := &config.Config{}

	runWizardInPTY(t, c, "y\n")

	if _, err := os.Stat(config.ConfigPath()); err != nil {
		t.Fatalf("the wizard did not write %s after the operator approved the save: %v", config.ConfigPath(), err)
	}
}
