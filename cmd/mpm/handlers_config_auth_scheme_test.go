// handlers_config_auth_scheme_test.go — the `auth_scheme` profile
// field on `mpm config profile set`.
//
// HAL-9001 added an explicit authentication-scheme override so an
// operator can pin the scheme for an endpoint that cannot be reliably
// discovered. The field must behave like the other profile fields:
//
//   - a supported scheme is stored verbatim;
//   - an unsupported scheme is refused with an actionable message and
//     NOTHING is written;
//   - an empty value clears the override, returning the profile to
//     derived resolution.
//
// The tests run against a redirected MPM_WORKSPACE so the write lands
// in a temp directory, never in the operator's store. The profile is
// deliberately incomplete (no model / base URL) so the post-save probe
// is not probe-eligible and no network call is made.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/config"
)

// isolatedWorkspace points MPM_WORKSPACE at a temp directory for the
// duration of the test.
func isolatedWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("HOME", t.TempDir())
	return ws
}

// profileWithAuthSchemeField returns a profile that is structurally
// incomplete, so the post-save probe stays a no-op.
func profileWithAuthSchemeField() *config.Config {
	return &config.Config{
		Profiles: map[string]config.Profile{
			"p": {Name: "p", Provider: "custom"},
		},
	}
}

// TestHandleProfileSet_AuthSchemeStoresTheScheme pins that a supported
// scheme is stored.
func TestHandleProfileSet_AuthSchemeStoresTheScheme(t *testing.T) {
	ws := isolatedWorkspace(t)
	cfg := profileWithAuthSchemeField()

	if code := handleProfileSet(cfg, "p", "auth_scheme", "bearer", false); code != 0 {
		t.Fatalf("handleProfileSet exit code = %d, want 0", code)
	}
	if got := cfg.Profiles["p"].AuthScheme; got != "bearer" {
		t.Errorf("profile AuthScheme = %q, want bearer", got)
	}

	// The value must survive the write, not just the in-memory struct.
	data, err := os.ReadFile(filepath.Join(ws, "mpm_config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), `"auth_scheme": "bearer"`) {
		t.Errorf("written config does not carry auth_scheme:\n%s", string(data))
	}
}

// TestHandleProfileSet_AuthSchemeRefusesUnsupportedValues pins that an
// unsupported scheme is refused and NOTHING is written.
//
// A silently-approximated scheme would present the credential in a
// shape the endpoint did not ask for, and the operator would have no
// way to tell from the configuration that it did not take effect.
func TestHandleProfileSet_AuthSchemeRefusesUnsupportedValues(t *testing.T) {
	for _, bad := range []string{"basic", "query-param", "Bearer2", "oauth2"} {
		t.Run(bad, func(t *testing.T) {
			ws := isolatedWorkspace(t)
			cfg := profileWithAuthSchemeField()

			if code := handleProfileSet(cfg, "p", "auth_scheme", bad, false); code == 0 {
				t.Fatalf("handleProfileSet accepted the unsupported auth_scheme %q", bad)
			}
			if got := cfg.Profiles["p"].AuthScheme; got != "" {
				t.Errorf("profile AuthScheme = %q, want it left empty after a refused write", got)
			}
			if _, err := os.Stat(filepath.Join(ws, "mpm_config.json")); err == nil {
				t.Error("a refused auth_scheme wrote the configuration file")
			}
		})
	}
}

// TestHandleProfileSet_AuthSchemeEmptyClearsTheOverride pins that an
// empty value clears the override.
//
// The override is an operator's temporary pin for an endpoint that
// cannot be discovered. There must be a way to remove it without
// deleting and recreating the profile.
func TestHandleProfileSet_AuthSchemeEmptyClearsTheOverride(t *testing.T) {
	isolatedWorkspace(t)
	cfg := profileWithAuthSchemeField()
	cfg.Profiles["p"] = config.Profile{Name: "p", Provider: "custom", AuthScheme: "bearer"}

	if code := handleProfileSet(cfg, "p", "auth_scheme", "", false); code != 0 {
		t.Fatalf("handleProfileSet exit code = %d, want 0", code)
	}
	if got := cfg.Profiles["p"].AuthScheme; got != "" {
		t.Errorf("profile AuthScheme = %q, want it cleared", got)
	}
}

// TestIsMaterialChange_AuthSchemeIsMaterial pins that changing the
// authentication scheme triggers the post-save verification.
//
// The scheme changes the bytes on the wire, so a cached "verified"
// result for the previous scheme would be presented as current.
func TestIsMaterialChange_AuthSchemeIsMaterial(t *testing.T) {
	p := config.Profile{Name: "p", Provider: "custom"}
	if !isMaterialChange(p, "auth_scheme") {
		t.Error("isMaterialChange(auth_scheme) = false, want true")
	}
}
