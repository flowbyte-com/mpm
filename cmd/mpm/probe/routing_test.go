// routing_test.go — Cover ResolveEffectiveTargets:
//   - single component binding
//   - two components resolving to same fingerprint dedup
//   - custom component / profile binding absent from DefaultCapabilities
//     still gets probed
//   - Components["embedding"] = "disabled" surfaces as disabled target
//   - dangling binding (binding to non-existent profile name)

package probe

import (
	"context"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

func TestResolveEffectiveTargets_SingleComponent(t *testing.T) {
	cfg := &config.Config{
		Profiles: map[string]config.Profile{
			"router": {Provider: "openai", Model: "gpt-4o-mini", BaseURL: "https://api.openai.com/v1"},
		},
		Components: map[string]string{"memory": "router"},
	}
	targets := ResolveEffectiveTargets(cfg)
	if len(targets) != 1 {
		t.Fatalf("targets len = %d, want 1", len(targets))
	}
	if targets[0].Component != "memory" {
		t.Fatalf("targets[0].Component = %q, want memory", targets[0].Component)
	}
	if targets[0].Profile == nil {
		t.Fatalf("profile should be non-nil for a valid binding")
	}
}

func TestResolveEffectiveTargets_DedupsByFingerprint(t *testing.T) {
	cfg := &config.Config{
		Profiles: map[string]config.Profile{
			"router": {Provider: "openai", Model: "gpt-4o-mini", BaseURL: "https://api.openai.com/v1"},
		},
		Components: map[string]string{
			"memory": "router",
			"critic": "router",
		},
	}
	targets := ResolveEffectiveTargets(cfg)
	if len(targets) != 1 {
		t.Fatalf("dedup targets len = %d, want 1 (same fingerprint collapses)", len(targets))
	}
	if len(targets[0].Components) != 2 {
		t.Fatalf("Components merged = %v, want 2 entries", targets[0].Components)
	}
}

func TestResolveEffectiveTargets_CustomBindingProbed(t *testing.T) {
	// Component absent from DefaultCapabilities.
	cfg := &config.Config{
		Profiles: map[string]config.Profile{
			"router": {Provider: "openai", Model: "gpt-4o-mini", BaseURL: "https://api.openai.com/v1"},
		},
		Components: map[string]string{"memory": "router"},
		Capabilities: map[string]string{
			"custom-vocab": "memory",
		},
	}
	targets := ResolveEffectiveTargets(cfg)
	if len(targets) != 1 {
		t.Fatalf("targets len = %d, want 1", len(targets))
	}
}

func TestResolveEffectiveTargets_DisabledBinding(t *testing.T) {
	cfg := &config.Config{
		Components: map[string]string{"embedding": "disabled"},
	}
	targets := ResolveEffectiveTargets(cfg)
	if len(targets) != 1 {
		t.Fatalf("targets len = %d, want 1", len(targets))
	}
	if !targets[0].Disabled {
		t.Fatalf("disabled binding should be Disabled=true")
	}
}

func TestResolveEffectiveTargets_DanglingBinding(t *testing.T) {
	cfg := &config.Config{
		Components: map[string]string{"critic": "does-not-exist"},
	}
	targets := ResolveEffectiveTargets(cfg)
	if len(targets) != 1 {
		t.Fatalf("targets len = %d, want 1", len(targets))
	}
	if targets[0].Profile != nil {
		t.Fatalf("dangling binding should have nil profile")
	}
}

func TestResolveEffectiveTargets_NoComponents(t *testing.T) {
	cfg := &config.Config{}
	targets := ResolveEffectiveTargets(cfg)
	if len(targets) != 0 {
		t.Fatalf("empty cfg should yield 0 targets; got %d", len(targets))
	}
}

func TestCanProbe_RequiresMaterialFields(t *testing.T) {
	cases := []struct {
		name string
		p    *config.Profile
		want bool
	}{
		{"nil", nil, false},
		{"all fields set", &config.Profile{Provider: "p", Model: "m", BaseURL: "u"}, true},
		{"missing provider", &config.Profile{Model: "m", BaseURL: "u"}, false},
		{"missing model", &config.Profile{Provider: "p", BaseURL: "u"}, false},
		{"missing base_url", &config.Profile{Provider: "p", Model: "m"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CanProbe(c.p); got != c.want {
				t.Fatalf("CanProbe(%v) = %v, want %v", c.p, got, c.want)
			}
		})
	}
}

// Disabled and active runs are surfaced distinctly so Doctor can render
// "intentionally disabled" separately from a runtime failure.
func TestRunOneProbe_DisabledSurfacesDistinctly(t *testing.T) {
	r := runOneProbe(context.Background(), ProbeTarget{
		Kind:      ProbeKindEmbedding,
		Component: "embedding",
		Disabled:  true,
	})
	if r.Status != ProbeDisabled {
		t.Fatalf("want ProbeDisabled, got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}
