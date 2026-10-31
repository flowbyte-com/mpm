package orchestration

// model_factory_timeout_test.go — the review path's timeout resolves
// through the substrate's policy.
//
// The ModelFactory builds a SynthClient from a profile. The timeout it
// puts on the wire must come from config.EffectiveTimeoutSeconds, so
// that a hand-edited zero or negative value becomes the default rather
// than an unbounded request.
//
// Go's http.Client treats a non-positive Timeout as UNLIMITED, so
// getting this wrong is not a "too short" failure — it is "no timeout
// at all", on a path that calls a provider an operator is paying for.

import (
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// TestDefaultModelFactory_TimeoutHonoursThePolicy is the truth table
// for the review path.
func TestDefaultModelFactory_TimeoutHonoursThePolicy(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		want       time.Duration
	}{
		{name: "an explicit value is used", configured: 90, want: 90 * time.Second},
		{name: "an unset value becomes the default", configured: 0, want: 300 * time.Second},
		{name: "a negative value becomes the default", configured: -1, want: 300 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factory := DefaultModelFactory()
			profile := config.Profile{
				Name:        "default",
				Provider:    "custom",
				Model:       "some-model",
				BaseURL:     "https://api.example.com/v1",
				APIKey:      "profile-key",
				TimeoutSecs: tc.configured,
			}
			mc, err := factory(profile)
			if err != nil {
				t.Fatalf("NewModelClient: %v", err)
			}
			sc := adapterSynthClient(t, mc)
			if sc.Timeout != tc.want {
				t.Errorf("Timeout = %v, want %v (configured timeout_seconds=%d)",
					sc.Timeout, tc.want, tc.configured)
			}
		})
	}
}
