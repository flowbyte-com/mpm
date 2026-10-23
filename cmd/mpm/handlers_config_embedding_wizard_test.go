// handlers_config_embedding_wizard_test.go — HAL-9001 Finding 2.
//
// The clean-install acceptance run configured an embedding endpoint
// through the wizard and then found the configuration unusable:
//
//	provider "custom" is not implemented
//	(supported: "ollama", "openai", "openai-compatible", "openrouter")
//
// The wizard had written provider="custom" — the vendor-neutral label
// the LLM path uses — but the embedding resolver selects its backend
// from the provider NAME, and "custom" selects none. The same run
// also saved a hand-typed endpoint ("http:://127.0.0.1:11434", a
// doubled colon) that nothing rejected.
//
// These tests drive the real wizard step with a scripted session and
// assert on the configuration it produces and on the runtime's own
// resolver — not on the wizard's printed text.
//
// Every session here is hermetic: the wizard writes only into an
// isolated workspace, and no endpoint is contacted.
package main

import (
	"bufio"
	"os"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
)

// isolatedEmbeddingWorkspace points the configuration surface at a
// throwaway workspace so a wizard run cannot write into the
// operator's configuration.
//
// It is deliberately self-contained: this file is also used as the
// RED baseline for the finding, where it must compile against the
// pre-fix tree on its own.
func isolatedEmbeddingWorkspace(t *testing.T) {
	t.Helper()
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

// TestWizardEmbeddingCustom_OllamaSelectionResolvesThroughTheRuntime
// pins the protocol-to-provider mapping.
//
// The operator picks a PROTOCOL; the profile must record the provider
// the runtime's embedding resolver understands. A protocol that maps
// to an unimplemented provider produces a configuration that reads
// fine and fails the moment anything uses it.
func TestWizardEmbeddingCustom_OllamaSelectionResolvesThroughTheRuntime(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := &config.Config{}

	// Protocol 2 = "Ollama (local)". No credential is prompted for
	// that protocol, so the session needs no secret.
	session := "2\nnomic-embed-text\nhttp://127.0.0.1:11434\n"
	wizardEmbeddingCustomWith(bufio.NewReader(strings.NewReader(session)), c)

	prof, ok := c.Profiles["embedding"]
	if !ok {
		t.Fatalf("the wizard did not create an embedding profile; profiles: %v", c.Profiles)
	}
	if prof.Provider != "ollama" {
		t.Errorf("embedding provider = %q, want %q — the protocol the operator selected must map to the provider the runtime implements",
			prof.Provider, "ollama")
	}

	// The configuration must survive the runtime's OWN resolver,
	// not merely look plausible.
	res := mpminternal.ResolveEmbeddingConfig(c)
	if res.Status != mpminternal.EmbeddingStatusConfigured {
		t.Errorf("embedding resolver status = %q, want %q (error: %v)",
			res.Status, mpminternal.EmbeddingStatusConfigured, res.LastError)
	}

	// The wizard's FINAL save is the only commit point (HAL-9001
	// Finding 4). What the final save writes is what matters.
	if err := config.SaveConfig(c); err != nil {
		t.Fatalf("save config: %v", err)
	}
	raw, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if !strings.Contains(string(raw), `"provider": "ollama"`) {
		t.Errorf("the saved configuration does not record the runtime provider; got:\n%s", raw)
	}
}

// TestWizardEmbeddingCustom_DoesNotWriteBeforeTheFinalSave pins the
// save semantics the onboarding flow depends on.
//
// The embedding step used to call config.SaveConfig itself. Because
// that happened in the MIDDLE of the wizard — before the operator's
// final "Save?" decision — declining the final save still committed
// the embedding profile to disk. The operator was shown "nothing
// saved" while their configuration had in fact changed.
//
// The step must therefore mutate the in-memory configuration only,
// and leave the single commit to the wizard's final save.
func TestWizardEmbeddingCustom_DoesNotWriteBeforeTheFinalSave(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := &config.Config{}

	session := "2\nnomic-embed-text\nhttp://127.0.0.1:11434\n"
	wizardEmbeddingCustomWith(bufio.NewReader(strings.NewReader(session)), c)

	// The profile was built in memory...
	if _, ok := c.Profiles["embedding"]; !ok {
		t.Fatalf("the wizard did not create an embedding profile; profiles: %v", c.Profiles)
	}

	// ...but nothing was written to disk.
	if _, err := os.Stat(config.ConfigPath()); err == nil {
		t.Errorf("the embedding step wrote the configuration before the wizard's final save; path: %s",
			config.ConfigPath())
	}
}

// TestWizardEmbeddingCustom_RejectsAMalformedEndpointURL pins that a
// malformed endpoint is refused BEFORE it reaches the configuration.
//
// The doubled-colon form is what a hand-typed "http://" becomes when
// the operator's keyboard inserts the colon twice. Nothing about it
// is secret and nothing about it is exotic — it is simply a typo
// that used to be saved and to fail much later.
func TestWizardEmbeddingCustom_RejectsAMalformedEndpointURL(t *testing.T) {
	isolatedEmbeddingWorkspace(t)
	c := &config.Config{}

	session := "2\nnomic-embed-text\nhttp:://127.0.0.1:11434\n"
	wizardEmbeddingCustomWith(bufio.NewReader(strings.NewReader(session)), c)

	if prof, ok := c.Profiles["embedding"]; ok {
		t.Errorf("the wizard accepted a malformed endpoint URL; profile recorded: %+v", prof)
	}
	if _, err := os.Stat(config.ConfigPath()); err == nil {
		t.Errorf("the wizard wrote a configuration file despite refusing the endpoint; path: %s", config.ConfigPath())
	}
}
