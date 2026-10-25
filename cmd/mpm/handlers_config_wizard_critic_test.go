// handlers_config_wizard_critic_test.go — HAL-9001 issue #62.
//
// The interactive wizard's critic step replaced an existing CONFIGURED
// critic profile with an empty one, destroying the stored provider,
// model, base URL and API key. The wizard's single commit point then
// persisted the replacement, so an operator who already had a working
// critic lost it by answering one question.
//
// What these tests pin:
//
//   - a configured profile is PRESERVED by default: answering "yes" to
//     "configure a separate critic model" and accepting the default
//     name does not destroy anything;
//   - replacing a configured profile requires an EXPLICIT confirmation
//     that names what is at stake, and the default answer preserves;
//   - the report says REPLACED, never "created", for a row that
//     already existed;
//   - a name that is free creates a new profile and leaves the
//     existing one alone;
//   - declining the step changes nothing;
//   - repeated runs do not progressively degrade the critic
//     configuration.
//
// The step is driven through an injected reader, so every prompt is
// answered deterministically. Nothing here writes the operator's
// configuration file: the step mutates the in-memory configuration and
// the wizard's single commit point persists it.
package main

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/flowbyte-com/mpm-core/config"
)

// configuredCriticConfig returns a configuration with a CONFIGURED
// critic profile bound to the critic component.
func configuredCriticConfig() *config.Config {
	return &config.Config{
		Profiles: map[string]config.Profile{
			"default": {Name: "default", Provider: "custom", Model: "gpt-4o", BaseURL: "https://api.example.com/v1"},
			"critic":  {Name: "critic", Provider: "anthropic", Model: "claude-opus", BaseURL: "https://api.anthropic.com/v1", APIKey: "sk-stored-critic-key"},
		},
		Components: map[string]string{"critic": "critic"},
	}
}

// TestWizardCritic_ConfiguredProfileIsPreservedByDefault is the
// headline correction.
//
// The operator answers "yes" to "configure a separate critic model",
// accepts the default profile name, and does NOT confirm the
// replacement. Nothing may be destroyed.
func TestWizardCritic_ConfiguredProfileIsPreservedByDefault(t *testing.T) {
	c := configuredCriticConfig()

	// "y" → configure a separate critic; "critic" → the default name;
	// "n" → do not replace the existing profile.
	out := captureStdout(t, func() {
		wizardCriticStep(c, bufio.NewReader(strings.NewReader("y\ncritic\nn\n")))
	})

	critic, ok := c.Profiles["critic"]
	if !ok {
		t.Fatal("the critic profile disappeared")
	}
	if critic.Provider != "anthropic" {
		t.Errorf("provider = %q, want %q (the configured profile must be preserved)", critic.Provider, "anthropic")
	}
	if critic.Model != "claude-opus" {
		t.Errorf("model = %q, want %q (the configured profile must be preserved)", critic.Model, "claude-opus")
	}
	if critic.BaseURL != "https://api.anthropic.com/v1" {
		t.Errorf("base url = %q, want the configured base url", critic.BaseURL)
	}
	if critic.APIKey != "sk-stored-critic-key" {
		t.Errorf("api key = %q, want the stored credential (the wizard must never discard it silently)", critic.APIKey)
	}
	if c.Components["critic"] != "critic" {
		t.Errorf("critic binding = %q, want %q", c.Components["critic"], "critic")
	}
	if !strings.Contains(out, "preserved") {
		t.Errorf("the report does not say the profile was preserved:\n%s", out)
	}
}

// TestWizardCritic_ReplacingAConfiguredProfileRequiresConfirmation pins
// that the destructive path exists but is explicit.
//
// The operator CONFIRMS the replacement, so it happens — and the report
// says "replaced", never "created", because the row already existed.
func TestWizardCritic_ReplacingAConfiguredProfileRequiresConfirmation(t *testing.T) {
	c := configuredCriticConfig()

	out := captureStdout(t, func() {
		wizardCriticStep(c, bufio.NewReader(strings.NewReader("y\ncritic\ny\n")))
	})

	critic, ok := c.Profiles["critic"]
	if !ok {
		t.Fatal("the critic profile disappeared")
	}
	if critic.APIKey != "" {
		t.Errorf("api key = %q, want empty (an explicitly confirmed replacement discards the credential)", critic.APIKey)
	}
	if !strings.Contains(out, "Replaced") {
		t.Errorf("the report does not say the profile was replaced:\n%s", out)
	}
	if strings.Contains(out, "Created empty profile") {
		t.Errorf("the report claims a fresh creation for a row that already existed:\n%s", out)
	}
	// The confirmation must name what is at stake.
	for _, want := range []string{"anthropic", "claude-opus", "api key"} {
		if !strings.Contains(out, want) {
			t.Errorf("the confirmation does not name %q:\n%s", want, out)
		}
	}
}

// TestWizardCritic_ANewNameCreatesANewProfile pins that the operator can
// add a SEPARATE critic without touching the configured one.
func TestWizardCritic_ANewNameCreatesANewProfile(t *testing.T) {
	c := configuredCriticConfig()

	out := captureStdout(t, func() {
		wizardCriticStep(c, bufio.NewReader(strings.NewReader("y\ncritic-2\n")))
	})

	critic := c.Profiles["critic"]
	if critic.APIKey != "sk-stored-critic-key" {
		t.Errorf("the existing critic's api key = %q, want the stored credential", critic.APIKey)
	}
	if critic.Model != "claude-opus" {
		t.Errorf("the existing critic's model = %q, want %q", critic.Model, "claude-opus")
	}
	fresh, ok := c.Profiles["critic-2"]
	if !ok {
		t.Fatal("the new profile was not created")
	}
	if fresh.Name != "critic-2" {
		t.Errorf("the new profile's name = %q, want %q", fresh.Name, "critic-2")
	}
	if fresh.APIKey != "" {
		t.Errorf("the new profile carries an api key: %q", fresh.APIKey)
	}
	if c.Components["critic"] != "critic-2" {
		t.Errorf("critic binding = %q, want %q", c.Components["critic"], "critic-2")
	}
	if !strings.Contains(out, "Created empty profile") {
		t.Errorf("the report does not say a profile was created:\n%s", out)
	}
}

// TestWizardCritic_DecliningTheStepChangesNothing pins that answering
// "no" leaves the configuration exactly as it was.
func TestWizardCritic_DecliningTheStepChangesNothing(t *testing.T) {
	c := configuredCriticConfig()

	out := captureStdout(t, func() {
		wizardCriticStep(c, bufio.NewReader(strings.NewReader("n\n")))
	})

	critic := c.Profiles["critic"]
	if critic.Provider != "anthropic" || critic.Model != "claude-opus" || critic.APIKey != "sk-stored-critic-key" {
		t.Errorf("the critic profile changed: %+v", critic)
	}
	if c.Components["critic"] != "critic" {
		t.Errorf("critic binding = %q, want %q", c.Components["critic"], "critic")
	}
	if !strings.Contains(out, "Critic unchanged") {
		t.Errorf("the report does not say the critic was left unchanged:\n%s", out)
	}
}

// TestWizardCritic_RepeatedRunsDoNotDegradeTheCritic pins that running
// the step twice does not progressively destroy the critic.
//
// The wizard is not idempotent as a whole; the critic step must at
// least not be the reason a second run costs the operator their
// configuration.
func TestWizardCritic_RepeatedRunsDoNotDegradeTheCritic(t *testing.T) {
	c := configuredCriticConfig()

	for i := 0; i < 3; i++ {
		captureStdout(t, func() {
			wizardCriticStep(c, bufio.NewReader(strings.NewReader("y\ncritic\nn\n")))
		})
	}

	critic := c.Profiles["critic"]
	if critic.Provider != "anthropic" {
		t.Errorf("after three runs the provider = %q, want %q", critic.Provider, "anthropic")
	}
	if critic.Model != "claude-opus" {
		t.Errorf("after three runs the model = %q, want %q", critic.Model, "claude-opus")
	}
	if critic.APIKey != "sk-stored-critic-key" {
		t.Errorf("after three runs the api key = %q, want the stored credential", critic.APIKey)
	}
	if c.Components["critic"] != "critic" {
		t.Errorf("after three runs the critic binding = %q, want %q", c.Components["critic"], "critic")
	}
}

// TestWizardCritic_AnEmptyProfileIsReplacedWithoutAConfirmation pins
// that an EMPTY profile carries nothing to lose.
//
// Replacing it is harmless, and the report must still not claim a fresh
// creation for a row that already existed.
func TestWizardCritic_AnEmptyProfileIsReplacedWithoutAConfirmation(t *testing.T) {
	c := &config.Config{
		Profiles: map[string]config.Profile{
			"default": {Name: "default", Provider: "custom", Model: "gpt-4o"},
			"critic":  {Name: "critic"},
		},
		Components: map[string]string{"critic": "critic"},
	}

	out := captureStdout(t, func() {
		wizardCriticStep(c, bufio.NewReader(strings.NewReader("y\ncritic\n")))
	})

	if _, ok := c.Profiles["critic"]; !ok {
		t.Fatal("the critic profile disappeared")
	}
	if strings.Contains(out, "Created empty profile") {
		t.Errorf("the report claims a fresh creation for a row that already existed:\n%s", out)
	}
}

// runWizardWithAnswersInPTY drives the interactive wizard through a
// pseudo-terminal with the operator's script and returns its exit code.
//
// The wizard refuses to run without a terminal, and a pipe cannot carry
// a multi-prompt session (each prompt builds its own buffered reader,
// which would swallow the rest of the script). A pseudo-terminal
// delivers one line at a time, so the script is answered prompt by
// prompt.
func runWizardWithAnswersInPTY(t *testing.T, c *config.Config, answers string, finalInput string) int {
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

	if _, err := io.WriteString(ptmx, answers+finalInput); err != nil {
		t.Fatalf("write the session to the pseudo-terminal: %v", err)
	}

	done := make(chan int, 1)
	go func() { done <- handleConfigInteractive(c) }()

	select {
	case code := <-done:
		return code
	case <-time.After(30 * time.Second):
		restoreStdin = false
		_ = ptmx.Close()
		t.Fatal("the wizard did not finish within 30s — it is probably waiting for an answer the script does not supply")
		return 1
	}
}

// criticSessionAnswers is the operator's script for a wizard run whose
// critic step is exercised:
//
//	Provider            → Custom
//	Protocol            → Ollama (no credential prompt)
//	Model               → left empty
//	Base URL            → the protocol's default
//	Timeout seconds     → the default
//	Embedding           → leave unchanged
//	Separate critic?    → yes
//	Profile name        → the conventional name
//	Replace it?         → no (the existing profile is preserved)
const criticSessionAnswers = "1\n3\n\n\n\n1\ny\ncritic\nn\n"

// TestWizardCritic_SessionPreservesTheConfiguredCritic is the
// session-level regression test.
//
// The whole wizard runs: the operator answers "yes" to "configure a
// separate critic model", accepts the default profile name, declines
// the replacement, and approves the final save. The configured critic
// must survive the save with its credential intact.
func TestWizardCritic_SessionPreservesTheConfiguredCritic(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := configuredCriticConfig()

	runWizardWithAnswersInPTY(t, c, criticSessionAnswers, "y\n")

	written, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		t.Fatalf("read the written configuration: %v", err)
	}
	if !strings.Contains(string(written), "sk-stored-critic-key") {
		t.Errorf("the written configuration lost the critic's stored credential:\n%s", written)
	}
	if !strings.Contains(string(written), "claude-opus") {
		t.Errorf("the written configuration lost the critic's model:\n%s", written)
	}
}

// TestWizardCritic_SessionCancellationWritesNothing pins that
// abandoning the run leaves the operator's configuration exactly as it
// was — including the configured critic.
//
// The wizard's commit point is the final "Save?"; a cancellation must
// not write, so the configured critic cannot be damaged by a run the
// operator abandoned.
func TestWizardCritic_SessionCancellationWritesNothing(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := configuredCriticConfig()

	runWizardWithAnswersInPTY(t, c, criticSessionAnswers, "\x04") // Ctrl-D: abort.

	if _, err := os.Stat(config.ConfigPath()); err == nil {
		t.Errorf("the wizard wrote %s after the operator aborted; an abort must leave the configuration exactly as it was",
			config.ConfigPath())
	}
}
