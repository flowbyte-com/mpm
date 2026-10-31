package synth

// timeout_test.go — the runtime half of the timeout policy.
//
// The policy is defined once, in internal/core/config/timeout.go. These
// tests assert that the RUNTIME honours it: every path that turns a
// configured value into a duration goes through the policy, so a
// hand-edited zero or negative value cannot produce a surprising
// request.
//
// They also pin the two INDEPENDENCES the policy depends on:
//
//   - the request timeout and the CONNECTION-VERIFICATION timeouts are
//     different things, and a slow provider must be reported as a
//     timeout rather than as an authentication failure;
//   - the MiniMax credential fix and the timeout fix do not interact.
//     A profile's timeout must not change which authentication scheme
//     is presented, and a profile's credential must not change the
//     timeout.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// writeTimeoutConfig writes a configuration whose default profile
// carries the given timeout_seconds.
func writeTimeoutConfig(t *testing.T, dir string, timeoutSeconds int) {
	t.Helper()
	// A negative or absent value is written literally — the point of
	// these tests is that a HAND-EDITED value is honoured by the
	// policy, not that the CLI would have accepted it.
	body := `{
  "profiles": {
    "default": {
      "provider": "minimax",
      "model": "TestModel-X",
      "base_url": "https://api.minimax.io/anthropic/v1",
      "api_key": "profile-key",
      "timeout_seconds": ` + itoa(timeoutSeconds) + `
    }
  }
}`
	cfgFile := filepath.Join(dir, "mpm_config.json")
	if err := os.WriteFile(cfgFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

// TestNewSynthClient_TimeoutHonoursThePolicy asserts that the client
// the runtime builds from a configuration resolves the timeout through
// the policy.
//
// The negative case is the load-bearing one: a hand-edited
// `timeout_seconds: -5` must become the default, not "no timeout".
func TestNewSynthClient_TimeoutHonoursThePolicy(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		want       time.Duration
	}{
		{name: "an explicit value is used", configured: 120, want: 120 * time.Second},
		{name: "an unset value becomes the default", configured: 0, want: 300 * time.Second},
		{name: "a negative value becomes the default", configured: -5, want: 300 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTimeoutConfig(t, dir, tc.configured)
			t.Setenv("MPM_WORKSPACE", dir)
			t.Setenv("HOME", dir)

			sc := NewSynthClient()
			if sc.Timeout != tc.want {
				t.Errorf("Timeout = %v, want %v (configured timeout_seconds=%d)",
					sc.Timeout, tc.want, tc.configured)
			}
		})
	}
}

// TestNewClientForProfile_TimeoutHonoursThePolicy asserts the same
// policy on the profile-scoped constructor.
//
// This is the constructor the MODEL FACTORY and the PROBE use. The
// probe passes its own timeout explicitly — see the independence test
// below — but the factory lets the profile decide.
func TestNewClientForProfile_TimeoutHonoursThePolicy(t *testing.T) {
	p := config.Profile{
		Name:        "default",
		Provider:    "minimax",
		Model:       "TestModel-X",
		BaseURL:     "https://api.minimax.io/anthropic/v1",
		APIKey:      "profile-key",
		TimeoutSecs: -5,
	}
	sc := NewClientForProfile(p, 0, 0)
	want := time.Duration(config.DefaultTimeoutSeconds) * time.Second
	if sc.Timeout != want {
		t.Errorf("Timeout = %v, want %v (a non-positive timeout argument falls back to the default)",
			sc.Timeout, want)
	}

	// An explicit caller-supplied timeout WINS — the probe depends on
	// this, and so does anything that needs a bounded request.
	sc = NewClientForProfile(p, 1500*time.Millisecond, 0)
	if sc.Timeout != 1500*time.Millisecond {
		t.Errorf("Timeout = %v, want 1.5s (an explicit timeout must win)", sc.Timeout)
	}
}

// TestTheTimeoutAndTheAuthenticationSchemeAreIndependent pins that the
// MiniMax credential fix and the timeout fix do not interact.
//
// The MiniMax Anthropic-compatible surface authenticates with
// `Authorization: Bearer`, and the live profile's `timeout_seconds: 3`
// is what made the operator's verification time out. Both are real
// properties of the same profile, and neither may influence the other:
// a timeout change must not alter the presented scheme, and a scheme
// change must not alter the timeout.
func TestTheTimeoutAndTheAuthenticationSchemeAreIndependent(t *testing.T) {
	for _, timeoutSeconds := range []int{3, 45, 300} {
		dir := t.TempDir()
		writeTimeoutConfig(t, dir, timeoutSeconds)
		t.Setenv("MPM_WORKSPACE", dir)
		t.Setenv("HOME", dir)

		sc := NewSynthClient()
		if sc.Timeout != time.Duration(timeoutSeconds)*time.Second {
			t.Errorf("Timeout = %v, want %ds", sc.Timeout, timeoutSeconds)
		}
		scheme, err := sc.AuthScheme()
		if err != nil {
			t.Fatalf("resolve the auth scheme: %v", err)
		}
		if scheme != AuthSchemeBearer {
			t.Errorf("auth scheme = %v, want %v (the timeout must not change the presented scheme)",
				scheme, AuthSchemeBearer)
		}
	}
}

// TestTheTimeoutAndTheCredentialAreIndependent pins the other
// direction: a profile's credential must not change the timeout.
//
// The credential is what the MiniMax 401 was about. A profile with no
// credential still resolves its timeout the same way, because the
// timeout is a property of the profile, not of the credential.
func TestTheTimeoutAndTheCredentialAreIndependent(t *testing.T) {
	dir := t.TempDir()
	body := `{
  "profiles": {
    "default": {
      "provider": "minimax",
      "model": "TestModel-X",
      "base_url": "https://api.minimax.io/anthropic/v1",
      "timeout_seconds": 45
    }
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "mpm_config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("MPM_WORKSPACE", dir)
	t.Setenv("HOME", dir)
	// The credential resolver falls back to the provider's own
	// environment variable. Clear it so this test observes the PROFILE,
	// not the host's credentials. (The assertion below reports only
	// whether a credential was found — never its value.)
	t.Setenv("MINIMAX_API_KEY", "")

	sc := NewSynthClient()
	if sc.Timeout != 45*time.Second {
		t.Errorf("Timeout = %v, want 45s (the credential must not change the timeout)", sc.Timeout)
	}
	if sc.APIKey != "" {
		// Deliberately does not print the value.
		t.Error("the client resolved a credential from the environment; the test did not isolate the environment")
	}
}
