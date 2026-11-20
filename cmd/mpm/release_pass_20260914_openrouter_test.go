// release_pass_20260914_openrouter_test.go — OpenRouter
// regressions for the 2026-09-14 final-simplification pass.
//
// 2026-09-14 final-simplification: openRouterCatalogForView
// (the live /api/v1/models discovery helper) is removed. The
// openrouter-branded menu path is gone; manual Custom +
// protocol configuration is the single public path. The
// runtime registry still resolves provider=openrouter for
// backwards compatibility (existing profiles load and wire
// unchanged).
//
// These tests pin the backwards-compat guarantees only. Live
// discovery tests are removed (no public surface consumes
// the catalog and no internal capability code depends on it).
//
// All tests hermetic via t.TempDir().

package main

import (
	stdlibexec "os/exec"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenRouter_BackwardsCompatRegistryResolution — the runtime
// registry still resolves provider=openrouter by string. An
// existing operator profile must continue to load and wire
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
// being rewritten as "custom". Runtime resolution by ID is
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

// stripLogNoise, lookupTestPath, and indexAnyLine live in
// release_pass_20260914_dashboard_test.go (same package); no
// duplicates here.

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
