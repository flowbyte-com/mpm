package orchestration

// model_factory_test.go — contract tests for the production
// ModelFactory's construction of the substrate's SynthClient.

import (
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/synth"
)

// adapterSynthClient digs the wrapped *synth.SynthClient out of the
// ModelClient the factory returns, so the test asserts on the value
// that will actually be placed on the wire.
func adapterSynthClient(t *testing.T, mc ModelClient) *synth.SynthClient {
	t.Helper()
	adapter, ok := mc.(*synthClientAdapter)
	if !ok {
		t.Fatalf("DefaultModelFactory returned %T, want *synthClientAdapter", mc)
	}
	if adapter.sc == nil {
		t.Fatal("synthClientAdapter wraps a nil SynthClient")
	}
	return adapter.sc
}

// TestDefaultModelFactory_IgnoresProfileMaxTokens pins the
// output-token-budget contract on the production review path.
//
// synth/client.go documents that output-token limits are NOT
// user-configurable: the substrate supplies synthInternalMaxTokens at
// wire time and any Profiles[...].MaxTokens value is ignored, so that
// a low configured value cannot silently truncate a long synthesis
// job. NewSynthClient honours that. The production ModelFactory did
// not — it copied profile.MaxTokens straight into the SynthClient
// literal, which is the value that reaches the wire as
// `max_tokens` (synth/client.go:360).
//
// This test is the regression for that: the factory's own package
// documentation advertised max_tokens as a configurable profile field,
// and a profile carrying max_tokens=10 would have capped every review
// response at ten tokens. The test asserts against the substrate
// constant rather than a literal so the two can never drift apart.
func TestDefaultModelFactory_IgnoresProfileMaxTokens(t *testing.T) {
	profile := config.Profile{
		Name:        "adversarial-low-budget",
		Provider:    "minimax",
		Model:       "TestModel-X",
		APIKey:      "test-key",
		BaseURL:     "https://api.test.example/anthropic/v1",
		MaxTokens:   10,
		TimeoutSecs: 120,
	}

	mc, err := DefaultModelFactory()(profile)
	if err != nil {
		t.Fatalf("DefaultModelFactory returned error: %v", err)
	}
	sc := adapterSynthClient(t, mc)

	if sc.MaxTokens != synth.InternalMaxTokens() {
		t.Fatalf("profile MaxTokens=10 reduced the synthesis output-token budget to %d; "+
			"the substrate contract (synth/client.go) makes output tokens non-user-configurable "+
			"and requires the internal ceiling %d",
			sc.MaxTokens, synth.InternalMaxTokens())
	}
}

// TestDefaultModelFactory_IgnoresZeroAndOversizedMaxTokens covers the
// two ends of the range. Zero must not mean "no limit" leaking
// through as a zero-valued wire field, and an oversized value must not
// raise the substrate ceiling — the budget is the substrate's to set,
// not the operator's, in both directions.
func TestDefaultModelFactory_IgnoresZeroAndOversizedMaxTokens(t *testing.T) {
	for _, maxTokens := range []int{0, 10, 1 << 20} {
		profile := config.Profile{
			Name:        "budget-edge",
			Provider:    "minimax",
			Model:       "TestModel-X",
			APIKey:      "test-key",
			BaseURL:     "https://api.test.example/anthropic/v1",
			MaxTokens:   maxTokens,
			TimeoutSecs: 120,
		}
		mc, err := DefaultModelFactory()(profile)
		if err != nil {
			t.Fatalf("DefaultModelFactory(MaxTokens=%d) returned error: %v", maxTokens, err)
		}
		sc := adapterSynthClient(t, mc)
		if sc.MaxTokens != synth.InternalMaxTokens() {
			t.Errorf("profile MaxTokens=%d produced wire MaxTokens=%d, want the substrate ceiling %d",
				maxTokens, sc.MaxTokens, synth.InternalMaxTokens())
		}
	}
}

// TestDefaultModelFactory_StillHonorsProfileConnectionFields is the
// counterweight: the fix targets the output-token budget only. Model,
// API key, base URL, and timeout are genuinely operator-configurable
// and must still flow from the profile, so a future "simplification"
// that over-corrects into ignoring the whole profile is caught here.
func TestDefaultModelFactory_StillHonorsProfileConnectionFields(t *testing.T) {
	profile := config.Profile{
		Name:        "connection-fields",
		Provider:    "minimax",
		Model:       "TestModel-X",
		APIKey:      "test-key",
		BaseURL:     "https://api.test.example/anthropic/v1",
		MaxTokens:   10,
		TimeoutSecs: 90,
	}

	mc, err := DefaultModelFactory()(profile)
	if err != nil {
		t.Fatalf("DefaultModelFactory returned error: %v", err)
	}
	sc := adapterSynthClient(t, mc)

	if sc.Model != "TestModel-X" {
		t.Errorf("Model = %q, want %q from the profile", sc.Model, "TestModel-X")
	}
	if sc.APIKey != "test-key" {
		t.Errorf("APIKey = %q, want %q from the profile", sc.APIKey, "test-key")
	}
	if sc.BaseURL != "https://api.test.example/anthropic/v1" {
		t.Errorf("BaseURL = %q, want the profile value", sc.BaseURL)
	}
	if sc.Timeout.Seconds() != 90 {
		t.Errorf("Timeout = %v, want 90s from the profile", sc.Timeout)
	}
}
