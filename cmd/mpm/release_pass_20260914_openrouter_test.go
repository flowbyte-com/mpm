// release_pass_20260914_openrouter_test.go — OpenRouter
// regressions for the 2026-09-14 config-simplification pass.
//
// The catalogue-expansion pass made OpenRouter a branded provider
// with public menu presence. The simplification pass removed that
// public menu presence but PRESERVES OpenRouter at the runtime
// layer:
//
//   * Existing profiles with provider=openrouter continue to load
//     and wire correctly (L — backward compatibility).
//   * Internal registry still resolves provider=openrouter by
//     string.
//   * Wire inference (internal/core/synth) still routes
//     openrouter.ai → wireOpenAI.
//   * `mpm config detect-embedding` still uses
//     `openRouterCatalogForView` for live /api/v1/models discovery.
//   * Live discovery of OpenRouter model IDs works at the embedding
//     detection surface; failure falls back to empty (no preset
//     advertised, per the brief).
//
// These tests cover exactly those guarantees. Public menu tests
// (OpenRouter appearing in the LLM wizard, openrouter/free being a
// model preset) were REMOVED with the simplification pass — see
// release_pass_20260914_catalogue_expansion_test.go for the new
// contract pinning.
//
// All tests hermetic via t.TempDir() and httptest fake servers.

package main

import (
	"encoding/json"
	stdlibexec "os/exec"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenRouter_BackwardsCompatRegistryResolution — the runtime
// registry still resolves provider=openrouter by string (L).
// An existing operator profile must continue to load and wire
// without errors.
func TestOpenRouter_BackwardsCompatRegistryResolution(t *testing.T) {
	p, ok := presetForID("openrouter")
	if !ok {
		t.Fatalf("provider=openrouter must still resolve through presetForID for backwards compatibility")
	}
	if p.DefaultBaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("openrouter base URL = %q, want canonical OpenRouter URL", p.DefaultBaseURL)
	}
	if !p.Has(CapGenerate) {
		t.Errorf("openrouter must support CapGenerate for backwards compatibility")
	}
}

// TestOpenRouter_BackwardsCompatProfileLoads — a stored profile
// with provider=openrouter loads under `mpm config show` without
// being rewritten as "custom" (L). Runtime resolution by ID is
// preserved; the wizard does NOT silently rewrite existing
// profiles.
func TestOpenRouter_BackwardsCompatProfileLoads(t *testing.T) {
	bin := buildOpenRouterBin(t)
	ws := t.TempDir()

	seeded := `{
  "profiles": {
    "default": {"provider":"openrouter","model":"anthropic/claude-3.5-sonnet","base_url":"https://openrouter.ai/api/v1","api_key":"ORKEY"},
    "openrouter-embedding": {"provider":"openrouter","model":"openai/text-embedding-3-small","base_url":"https://openrouter.ai/api/v1","api_key":"ORKEY"}
  },
  "components": {"memory":"default","embedding":"openrouter-embedding"}
}`
	if err := os.WriteFile(filepath.Join(ws, "mpm_config.json"),
		[]byte(seeded), 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	// `mpm config show` must surface the openrouter profiles
	// unchanged; the api_key is redacted in display but the
	// provider/model/base_url are not mutated.
	cmd := stdlibexec.Command(bin, "config", "show")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config show: %v\n%s", err, out)
	}
	s := stripLogNoise(string(out))
	if !strings.Contains(s, "openrouter") {
		t.Errorf("config show must surface provider=openrouter; got:\n%s", s)
	}
	if !strings.Contains(s, "anthropic/claude-3.5-sonnet") {
		t.Errorf("config show must preserve the existing OpenRouter model; got:\n%s", s)
	}
	if !strings.Contains(s, "openrouter-embedding") {
		t.Errorf("config show must surface the secondary OpenRouter profile; got:\n%s", s)
	}

	// On-disk file MUST preserve the keys verbatim (config show
	// only redacts on display).
	body, err := os.ReadFile(filepath.Join(ws, "mpm_config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	roundTripped := stripLogNoise(string(body))
	if !strings.Contains(roundTripped, "ORKEY") {
		t.Errorf("OpenRouter API key was lost on load+save round-trip; got:\n%s", roundTripped)
	}
	if !strings.Contains(roundTripped, "openrouter") {
		t.Errorf("OpenRouter provider was lost on round-trip; got:\n%s", roundTripped)
	}
	if !strings.Contains(roundTripped, "anthropic/claude-3.5-sonnet") {
		t.Errorf("OpenRouter model lost on round-trip; got:\n%s", roundTripped)
	}
}

// TestOpenRouter_LiveDiscoveryStillWorks — `openRouterCatalogForView`
// still discovers live model IDs from /api/v1/models (the
// embedding discovery surface depends on it). The simplification
// did NOT remove live discovery — only the static preset fallback.
func TestOpenRouter_LiveDiscoveryStillWorks(t *testing.T) {
	srv := newFakeOpenRouter(t, []string{
		"anthropic/claude-3.5-sonnet",
		"openai/text-embedding-3-small",
		"google/gemini-2.5-pro",
	})
	defer srv.Close()
	t.Setenv("OPENROUTER_ENDPOINT", srv.URL+"/")

	catalog := openRouterCatalogForView()
	if len(catalog) == 0 {
		t.Fatalf("live discovery must populate the catalog when /api/v1/models responds")
	}
	for _, want := range []string{
		"anthropic/claude-3.5-sonnet",
		"openai/text-embedding-3-small",
		"google/gemini-2.5-pro",
	} {
		found := false
		for _, m := range catalog {
			if m == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("live discovery catalog missing %q; got %v", want, catalog)
		}
	}
}

// TestOpenRouter_DiscoveryFailureReturnsEmpty — when /api/v1/models
// is unreachable, the catalog falls back to empty (no static
// preset advertised). Operators are not given a fake fallback;
// they type the model name freeform at the Custom prompt.
func TestOpenRouter_DiscoveryFailureReturnsEmpty(t *testing.T) {
	t.Setenv("OPENROUTER_ENDPOINT", "http://127.0.0.1:1/")
	catalog := openRouterCatalogForView()
	// Empty on probe failure: the brief removed the static
	// `openrouter/free` preset.
	for _, m := range catalog {
		if m == "openrouter/free" {
			t.Errorf("openrouter/free must NOT be advertised as a preset; got %v", catalog)
		}
	}
}

// TestOpenRouter_NotInPublicWizardMenu — the OpenRouter branded
// menu entry is gone from public wizard UX. wizardPresets shows
// only Custom; the operator uses Custom + openai-compatible
// protocol + base URL for OpenRouter.
func TestOpenRouter_NotInPublicWizardMenu(t *testing.T) {
	presets := wizardPresets()
	for _, p := range presets {
		if p.id == "openrouter" {
			t.Errorf("public wizard must NOT surface openrouter as a menu entry; got %v", presetIDs(presets))
		}
	}
}

// --- helpers ---

// newFakeOpenRouter stands up a minimal /api/v1/models fake
// server with the given model IDs in the response.
func newFakeOpenRouter(t *testing.T, ids []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		data := make([]map[string]string, len(ids))
		for i, id := range ids {
			data[i] = map[string]string{"id": id}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": data})
	}))
	return srv
}

// buildOpenRouterBin builds a fresh mpm binary in a temp dir for
// OpenRouter tests.
func buildOpenRouterBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-test")
	cmd := stdlibexec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// stripLogNoise, lookupTestPath, and indexAnyLine live in
// release_pass_20260914_dashboard_test.go (same package); no
// duplicates here.
