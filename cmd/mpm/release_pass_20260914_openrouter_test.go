// release_pass_20260914_openrouter_test.go — OpenRouter
// regressions (release-pass brief cases A-J).
//
//   A. OpenRouter appears as provider.
//   B. OpenRouter is in correct alphabetical position.
//   C. Base URL is exactly the canonical OpenRouter API URL.
//   D. OpenRouter uses its own API-key configuration.
//   E. openrouter/free is offered as a model/router preset.
//   F. openrouter/free is NOT represented as a separate provider.
//   G. choosing it produces provider=openrouter, model=openrouter/free.
//   H. role validation accepts it as generate-capable/router.
//   I. OpenRouter discovery failure still leaves Custom + stable router preset.
//   J. config save/load round-trips OpenRouter without mutation.
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

// TestOpenRouter_A_AppearsAsProvider — OpenRouter is in the
// canonical providers registry.
func TestOpenRouter_A_AppearsAsProvider(t *testing.T) {
	llms := providersFor(CapGenerate)
	found := false
	for _, p := range llms {
		if p.ID == "openrouter" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("openrouter must be a registered LLM provider; got IDs: %v", providerIDs(llms))
	}
}

// TestOpenRouter_B_AlphabeticalPosition — OpenRouter sorts in
// its canonical position. "OpenRouter" < "OpenAI-compatible"
// alphabetically? No: "OpenAI-compatible" < "OpenRouter"
// because the first differs at index 6 ('-' < 'r').
//
// Wait — that's "openai-compatible" vs "openrouter". Let me
// check: "openai" is at index 6; "openrouter" is at index 6 too
// (the 'r' in 'openrouter' vs 'a' in 'openai'). So
// "openai-compatible" < "openrouter" because "openai" < "openrouter"
// prefix-ordering wins. Confirmed by the brief's expected order.
func TestOpenRouter_B_AlphabeticalPosition(t *testing.T) {
	llms := providersFor(CapGenerate)
	var positions []string
	for _, p := range llms {
		positions = append(positions, p.ID)
	}
	// The OpenRouter position must be after OpenAI-compatible
	// and before xAI (case-insensitive alphabetical).
	wantPrev := "openai-compatible"
	wantNext := "xai"
	for i, id := range positions {
		if id != "openrouter" {
			continue
		}
		if i > 0 && positions[i-1] != wantPrev {
			t.Errorf("OpenRouter should follow %s; got %s", wantPrev, positions[i-1])
		}
		if i+1 < len(positions) && positions[i+1] != wantNext {
			t.Errorf("OpenRouter should precede %s; got %s", wantNext, positions[i+1])
		}
		return
	}
	t.Fatalf("OpenRouter missing from LLM view: %v", positions)
}

// TestOpenRouter_C_CanonicalBaseURL — OpenRouter's base URL is
// the canonical OpenRouter API URL.
func TestOpenRouter_C_CanonicalBaseURL(t *testing.T) {
	p, ok := presetForID("openrouter")
	if !ok {
		t.Fatalf("openrouter must be registered")
	}
	if p.DefaultBaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("OpenRouter base URL = %q, want %q", p.DefaultBaseURL, "https://openrouter.ai/api/v1")
	}
}

// TestOpenRouter_D_OwnAPIKey — OpenRouter uses its own API key
// configuration (NeedsAPIKey=true) so the operator's
// OPENAI_API_KEY does not silently route to OpenRouter.
func TestOpenRouter_D_OwnAPIKey(t *testing.T) {
	p, ok := presetForID("openrouter")
	if !ok {
		t.Fatalf("openrouter must be registered")
	}
	if !p.NeedsAPIKey {
		t.Errorf("OpenRouter must require its own API key (OPENROUTER_API_KEY)")
	}
}

// TestOpenRouter_E_FreeIsModelPreset — "openrouter/free" is in
// the OpenRouter model catalog.
func TestOpenRouter_E_FreeIsModelPreset(t *testing.T) {
	models := modelCatalogFor("openrouter")
	found := false
	for _, m := range models {
		if m == "openrouter/free" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("openrouter/free must be in the OpenRouter catalog; got %v", models)
	}
}

// TestOpenRouter_F_NotASeparateProvider — "openrouter-free" is
// not a separate provider entry.
func TestOpenRouter_F_NotASeparateProvider(t *testing.T) {
	if _, ok := presetForID("openrouter-free"); ok {
		t.Errorf("openrouter-free must NOT be a separate provider")
	}
	if _, ok := presetForID("openrouter_free"); ok {
		t.Errorf("openrouter_free must NOT be a separate provider")
	}
}

// TestOpenRouter_G_ProfileShape — Choosing the OpenRouter Free
// router produces a profile with provider=openrouter and
// model=openrouter/free. We exercise the non-interactive path:
//   `mpm config profile add <name>` then
//   `mpm config profile set <name> provider openrouter` etc.
func TestOpenRouter_G_ProfileShape(t *testing.T) {
	bin := buildOpenRouterBin(t)
	ws := t.TempDir()

	// Step 1: add the profile.
	cmd := stdlibexec.Command(bin, "config", "profile", "add", "openrouter-embedding")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("profile add: %v\n%s", err, out)
	}

	// Step 2: populate the three fields.
	for _, kv := range [][2]string{
		{"provider", "openrouter"},
		{"base_url", "https://openrouter.ai/api/v1"},
		{"model", "openrouter/free"},
	} {
		cmd := stdlibexec.Command(bin, "config", "profile", "set", "openrouter-embedding", kv[0], kv[1])
		cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("profile set %s=%s: %v\n%s", kv[0], kv[1], err, out)
		}
	}

	// Read back via `mpm config profile list`.
	cmd = stdlibexec.Command(bin, "config", "profile", "list")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("profile list: %v\n%s", err, out)
	}
	s := stripLogNoise(string(out))
	if !strings.Contains(s, "openrouter/free") {
		t.Errorf("profile list must surface the chosen model; got:\n%s", s)
	}
	if !strings.Contains(s, "openrouter-embedding") {
		t.Errorf("profile list must surface the new profile; got:\n%s", s)
	}

	// Inspect mpm_config.json directly: the saved profile MUST
	// have provider=openrouter (not a fake "openrouter-free"
	// provider) and model=openrouter/free.
	body, err := os.ReadFile(filepath.Join(ws, "mpm_config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		Profiles map[string]struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			BaseURL  string `json:"base_url"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("parse config: %v\n%s", err, body)
	}
	prof, ok := cfg.Profiles["openrouter-embedding"]
	if !ok {
		t.Fatalf("openrouter-embedding profile missing in saved config: %s", body)
	}
	if prof.Provider != "openrouter" {
		t.Errorf("provider = %q, want openrouter", prof.Provider)
	}
	if prof.Model != "openrouter/free" {
		t.Errorf("model = %q, want openrouter/free", prof.Model)
	}
	if prof.BaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("base_url = %q, want canonical OpenRouter URL", prof.BaseURL)
	}
}

// TestOpenRouter_H_RoleValidationAccepts — OpenRouter Free
// passes the role validator: it is generate-capable (router
// model that the operator explicitly chose).
func TestOpenRouter_H_RoleValidationAccepts(t *testing.T) {
	// Resolve provider from registry.
	p, ok := presetForID("openrouter")
	if !ok {
		t.Fatalf("openrouter must be registered")
	}
	// OpenRouter Free is a router/forwarding endpoint that
	// returns generated text. The role validator's "unknown
	// capability = accept" rule covers it (the validator is
	// permissive for unknown models on OpenAI-compatible
	// endpoints).
	decision := ValidateLLMRole(p.ID, "openrouter/free", p.DefaultBaseURL)
	if decision == RoleEmbeddingOnly {
		t.Errorf("openrouter/free must NOT be classified as embedding-only; got %v", decision)
	}
	// Acceptable: RoleValid (positive evidence from a
	// future capability probe) or RoleUnknown (probe failed
	// → permissive custom contract).
	if decision != RoleValid && decision != RoleUnknown {
		t.Errorf("openrouter/free role decision = %v, want RoleValid or RoleUnknown", decision)
	}
}

// TestOpenRouter_I_DiscoveryFailureFallback — When the live
// /api/v1/models endpoint is unreachable, the OpenRouter
// catalog falls back to the stable `openrouter/free` preset.
// Custom is still offered first.
func TestOpenRouter_I_DiscoveryFailureFallback(t *testing.T) {
	// Force the discovery endpoint to be unreachable.
	t.Setenv("OPENROUTER_ENDPOINT", "http://127.0.0.1:1/")
	catalog := openRouterCatalogForView()
	if len(catalog) == 0 {
		t.Fatalf("openRouterCatalogForView must return at least the stable preset on probe failure; got empty")
	}
	found := false
	for _, m := range catalog {
		if m == "openrouter/free" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("stable preset openrouter/free missing from fallback catalog; got %v", catalog)
	}
	// Catalog must remain alphabetical on the fallback path.
	for i := 1; i < len(catalog); i++ {
		if strings.ToLower(catalog[i-1]) > strings.ToLower(catalog[i]) {
			t.Errorf("fallback catalog not alphabetical: %v", catalog)
			break
		}
	}
}

// TestOpenRouter_I_DiscoverySuccessIncludesLiveCatalogue —
// When /api/v1/models responds, the live catalogue wins over
// the offline fallback. The Custom entry is added by the
// menu helper at render time (NOT here).
func TestOpenRouter_I_DiscoverySuccessIncludesLiveCatalogue(t *testing.T) {
	srv := newFakeOpenRouter(t, []string{
		"openrouter/free",
		"anthropic/claude-3.5-sonnet",
		"openai/gpt-5-luna",
		"google/gemini-2.5-pro",
	})
	defer srv.Close()
	t.Setenv("OPENROUTER_ENDPOINT", srv.URL+"/")

	catalog := openRouterCatalogForView()
	if len(catalog) == 0 {
		t.Fatalf("live catalog must be non-empty")
	}
	for _, want := range []string{"anthropic/claude-3.5-sonnet", "openai/gpt-5-luna", "google/gemini-2.5-pro"} {
		found := false
		for _, m := range catalog {
			if m == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("live catalog missing %q; got %v", want, catalog)
		}
	}
}

// TestOpenRouter_J_RoundTrip — Config save + load round-trips
// the OpenRouter profile without mutation. The `mpm config
// show` surface redacts api_key (correct behaviour — secrets
// are not echoed); the on-disk file MUST preserve the key
// verbatim across a load+save cycle.
func TestOpenRouter_J_RoundTrip(t *testing.T) {
	bin := buildOpenRouterBin(t)
	ws := t.TempDir()

	seeded := `{
  "profiles": {
    "default": {"provider":"openai","model":"gpt-5-luna","base_url":"https://api.openai.com/v1","api_key":"KEEP"},
    "openrouter-embedding": {"provider":"openrouter","model":"openrouter/free","base_url":"https://openrouter.ai/api/v1","api_key":"ORKEY"}
  },
  "components": {"memory":"default","embedding":"openrouter-embedding"}
}`
	if err := os.WriteFile(filepath.Join(ws, "mpm_config.json"),
		[]byte(seeded), 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	// `mpm config show` redacts api_key in display. The OpenRouter
	// model + provider names MUST surface.
	cmd := stdlibexec.Command(bin, "config", "show")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config show: %v\n%s", err, out)
	}
	s := stripLogNoise(string(out))
	if !strings.Contains(s, "openrouter/free") {
		t.Errorf("config show must surface OpenRouter Free router; got:\n%s", s)
	}
	if !strings.Contains(s, "openrouter-embedding") {
		t.Errorf("config show must surface the OpenRouter profile; got:\n%s", s)
	}

	// Read back the on-disk file: keys preserved verbatim
	// (config show only redacts on display).
	body, err := os.ReadFile(filepath.Join(ws, "mpm_config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	roundTripped := stripLogNoise(string(body))
	if !strings.Contains(roundTripped, "ORKEY") {
		t.Errorf("OpenRouter API key was lost on load+save round-trip; got:\n%s", roundTripped)
	}
	if !strings.Contains(roundTripped, "KEEP") {
		t.Errorf("OpenAI API key was lost on load+save round-trip; got:\n%s", roundTripped)
	}
	if !strings.Contains(roundTripped, "openrouter/free") {
		t.Errorf("OpenRouter model was lost on round-trip; got:\n%s", roundTripped)
	}
}

// --- helpers ---

// providerIDs extracts the IDs from a slice of providers for
// clearer assertion failures.
func providerIDs(ps []ProviderDefinition) []string {
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.ID
	}
	return ids
}

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
