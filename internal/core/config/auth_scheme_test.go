// auth_scheme_test.go — the configuration layer's validation of the
// operator's authentication-scheme override (issue #68).
//
// The override used to be validated only where it was TYPED (the CLI).
// The configuration FILE was never validated: a hand-edited
// mpm_config.json carrying `auth_scheme: "basic"` was silently dropped
// at resolution time, and the endpoint was authenticated with a derived
// scheme instead.

package config

import (
	"strings"
	"testing"
)

// TestValidateAuthScheme pins what a usable scheme value is.
//
// An empty value is valid: it means "no override".
func TestValidateAuthScheme(t *testing.T) {
	valid := []string{"", "bearer", "x-api-key", "x_api_key", "xapikey", "api-key", "none", "no-auth", "noauth", "BEARER", " Bearer "}
	for _, raw := range valid {
		if err := ValidateAuthScheme(raw); err != nil {
			t.Errorf("ValidateAuthScheme(%q) = %v; want nil", raw, err)
		}
	}
	invalid := []string{"basic", "Basic", "oauth2", "bearer2", "xapikey2", "none-ish"}
	for _, raw := range invalid {
		if err := ValidateAuthScheme(raw); err == nil {
			t.Errorf("ValidateAuthScheme(%q) = nil; want an error naming the supported set", raw)
		}
	}
}

// TestValidateAuthScheme_NamesTheSupportedSet pins that the refusal is
// actionable: it names the offending value AND the supported set.
func TestValidateAuthScheme_NamesTheSupportedSet(t *testing.T) {
	err := ValidateAuthScheme("basic")
	if err == nil {
		t.Fatal("ValidateAuthScheme(\"basic\") = nil; want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "basic") {
		t.Errorf("the error does not name the offending value: %v", err)
	}
	for _, want := range SupportedAuthSchemes() {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not name the supported scheme %q: %v", want, err)
		}
	}
}

// TestConfig_InvalidAuthSchemes pins that the configuration layer
// reports every profile whose auth_scheme is unusable.
//
// The report is computed from the authoritative configuration on read —
// nothing about it is persisted.
func TestConfig_InvalidAuthSchemes(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]Profile{
			"good":     {Name: "good", AuthScheme: "bearer"},
			"unset":    {Name: "unset"},
			"bad":      {Name: "bad", AuthScheme: "basic"},
			"also-bad": {Name: "also-bad", AuthScheme: "oauth2"},
		},
	}
	issues := cfg.InvalidAuthSchemes()
	if len(issues) != 2 {
		t.Fatalf("InvalidAuthSchemes() returned %d issue(s) (%v); want 2", len(issues), issues)
	}
	joined := strings.Join(issues, "\n")
	for _, want := range []string{"bad", "basic", "also-bad", "oauth2"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the issues do not name %q: %v", want, issues)
		}
	}
	// A usable profile is never reported.
	if strings.Contains(joined, "good") || strings.Contains(joined, "unset") {
		t.Errorf("a usable profile was reported as an issue: %v", issues)
	}
}

// TestConfig_InvalidAuthSchemes_NilConfig pins that the helper is safe
// on a nil configuration.
func TestConfig_InvalidAuthSchemes_NilConfig(t *testing.T) {
	var cfg *Config
	if issues := cfg.InvalidAuthSchemes(); len(issues) != 0 {
		t.Errorf("InvalidAuthSchemes() on a nil config = %v; want none", issues)
	}
}
