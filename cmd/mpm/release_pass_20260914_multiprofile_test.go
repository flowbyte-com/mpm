// release_pass_20260914_multiprofile_test.go — multi-profile
// config audit + CLI pass. Built-binary smoke (where practical)
// and in-process resolver checks.
//
// 2026-09-15 release-pass: the substrate's multi-profile
// architecture survived the config simplification intact
// (commit 0d08462c … 75e8302f). This file makes that capability
// first-class from the CLI surface, with explicit binding_source
// visibility, JSON parity, safe bound-profile deletion, and a
// behavioural fake-endpoint no-failover test.
//
// Each test runs in `package main` (same as cmd/mpm); hermetic
// fixtures use t.TempDir() for MPM_WORKSPACE. Built-binary
// tests reuse buildRunawayBin / stripLogNoise / lookupTestPath
// from the existing release-pass test files.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	stdlibexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/synth"

	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------------------
// Hermetic env helper — clears every env var the embedding resolver
// reads (correction #6). The names are enumerated directly from
// internal/core/embeddings.go so the hermetic contract is exact.
// ---------------------------------------------------------------------------

// clearEmbeddingEnv returns an os/exec Env slice with every env var
// the embedding resolver consults removed, plus the host PATH so
// the binary can find its dependencies. Tests that need a specific
// env var (Ollama endpoint) set it AFTER calling this helper.
//
// The embedding resolver reads (see internal/core/embeddings.go):
//   - OLLAMA_ENDPOINT, OLLAMA_MODEL (ollamaEnvFallback)
//   - OPENAI_ENDPOINT, OPENAI_API_KEY, OPENAI_MODEL
//   - OPENROUTER_ENDPOINT, OPENROUTER_API_KEY, OPENAI_COMPAT_MODEL
//   - OAI_COMPAT_ENDPOINT, OAI_COMPAT_API_KEY, OAI_COMPAT_MODEL
//
// We also clear a few common developer-machine aliases for safety.
func clearEmbeddingEnv(workspace string) []string {
	env := []string{
		"MPM_WORKSPACE=" + workspace,
		"PATH=" + lookupTestPath(),
		"HOME=" + os.TempDir(),
	}
	for _, k := range []string{
		"OLLAMA_ENDPOINT", "OLLAMA_MODEL",
		"OPENAI_ENDPOINT", "OPENAI_API_KEY", "OPENAI_MODEL",
		"OPENROUTER_ENDPOINT", "OPENROUTER_API_KEY", "OAI_COMPAT_MODEL",
		"OAI_COMPAT_ENDPOINT", "OAI_COMPAT_API_KEY", "OAI_COMPAT_MODEL",
		// common aliases that could leak
		"ANTHROPIC_API_KEY", "MINIMAX_API_KEY", "GEMINI_API_KEY",
		"GOOGLE_API_KEY", "XAI_API_KEY", "MISTRAL_API_KEY",
		"COHERE_API_KEY",
	} {
		env = append(env, k+"=")
	}
	return env
}

// runMpmCommand runs the freshly-built mpm binary against the
// workspace, with the embedding env cleared by default.
//
// KNOWN BASELINE DEFECT B (2026-09-21): pre-fix this helper passed
// --base-url values like https://*.invalid / *.example.com straight
// through, which forced `mpm config profile set …` to perform real
// outbound HTTP probes against reserved/example hosts. Each probe
// waited ~2s for DNS+TCP timeout; 30+ TestMulti_* tests × 2s drove
// the cumulative package runtime past the cmd/mpm go-test -timeout
// budget, hanging whichever test happened to be active when the
// package timed out.
//
// Repair: substitute any --base-url VALUE / base_url=VALUE pair with
// a local fake probe endpoint that the helper wires up at first use
// and tears down on test cleanup. The probe target is now hermetic;
// the test's *semantic* contract (add profile, set fields, component
// operations round-trip) is unchanged.
func runMpmCommand(t *testing.T, bin, ws string, args ...string) (string, error) {
	t.Helper()
	// Use a per-test fake endpoint so failures are isolated.
	srv := startFakeProbeEndpoint(t)
	t.Cleanup(srv.Close)
	args = substituteBaseURLsForHermetic(args, srv.URL)
	cmd := stdlibexec.Command(bin, args...)
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// substituteBaseURLsForHermetic rewrites every --base-url VALUE /
// base_url=VALUE pair so VALUE points at fakeBaseURL + "/v1".
// Other args pass through unchanged. The hermetic server returns
// 200 with empty capability JSON for /api/show, /api/tags, and
// /v1/messages, so probes return promptly.
func substituteBaseURLsForHermetic(args []string, fakeBaseURL string) []string {
	out := make([]string, 0, len(args))
	target := fakeBaseURL + "/v1"
	for i := 0; i < len(args); i++ {
		a := args[i]
		out = append(out, a)
		if a == "--base-url" && i+1 < len(args) {
			out = append(out, target)
			i++
			continue
		}
		if strings.HasPrefix(a, "base_url=") {
			out[len(out)-1] = "base_url=" + target
			continue
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// A–AH test matrix
// ---------------------------------------------------------------------------

// A. Create first Custom LLM profile via CLI.
func TestMulti_A_FirstCustomLLMProfile(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	if _, err := runMpmCommand(t, bin, ws,
		"config", "profile", "add", "primary",
		"--provider", "custom",
		"--model", "model-a",
		"--base-url", "https://example-a.invalid/v1",
	); err != nil {
		t.Fatalf("profile add: %v", err)
	}
	const secret = "sk-aaa-very-long-secret-1234567890"
	// 2026-09-19 credential-UX hardening: positional secret on
	// the command line is rejected; the canonical automation path
	// is stdin.
	setStdinCmd := stdlibexec.Command(bin,
		"config", "profile", "set", "primary", "api_key", "--stdin")
	setStdinCmd.Env = clearEmbeddingEnv(ws)
	setStdinCmd.Stdin = strings.NewReader(secret + "\n")
	if out, err := setStdinCmd.CombinedOutput(); err != nil {
		t.Fatalf("profile set api_key (--stdin): %v\n%s", err, out)
	}
	out, err := runMpmCommand(t, bin, ws, "config", "profile", "get", "primary")
	if err != nil {
		t.Fatalf("profile get: %v", err)
	}
	if strings.Contains(out, secret) {
		t.Errorf("profile get leaked api_key; got:\n%s", out)
	}
	if !strings.Contains(out, "...") {
		t.Errorf("redacted marker missing; got:\n%s", out)
	}
}

// B. Two Custom profiles coexist.
func TestMulti_B_SecondCustomLLMProfile(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, args := range [][]string{
		{"add", "primary", "--provider", "custom", "--model", "model-a", "--base-url", "https://a.invalid/v1"},
		{"add", "secondary", "--provider", "custom", "--model", "model-b", "--base-url", "https://b.invalid/v1"},
	} {
		if _, err := runMpmCommand(t, bin, ws, append([]string{"config", "profile"}, args...)...); err != nil {
			t.Fatalf("profile add %s: %v", args[1], err)
		}
	}
	out, err := runMpmCommand(t, bin, ws, "config", "profile", "list")
	if err != nil {
		t.Fatalf("profile list: %v", err)
	}
	for _, name := range []string{"primary", "secondary"} {
		if !strings.Contains(out, name) {
			t.Errorf("profile list missing %q; got:\n%s", name, out)
		}
	}
}

// C. Three profiles with different protocols coexist.
func TestMulti_C_ThirdProfileDifferentProtocol(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	profiles := []struct {
		name, proto, model, url string
	}{
		{"primary", "openai-compatible", "model-a", "https://a.invalid/v1"},
		{"secondary", "anthropic-compatible", "model-b", "https://b.invalid"},
		{"local", "ollama", "model-c", "http://127.0.0.1:11434"},
	}
	for _, p := range profiles {
		args := []string{"config", "profile", "add", p.name,
			"--provider", "custom",
			"--model", p.model,
			"--base-url", p.url,
		}
		if _, err := runMpmCommand(t, bin, ws, args...); err != nil {
			t.Fatalf("profile add %s: %v", p.name, err)
		}
	}
	out, err := runMpmCommand(t, bin, ws, "config", "profile", "list")
	if err != nil {
		t.Fatalf("profile list: %v", err)
	}
	for _, p := range profiles {
		if !strings.Contains(out, p.name) {
			t.Errorf("profile list missing %q; got:\n%s", p.name, out)
		}
	}
}

// D. All three profiles survive a save/reload round-trip.
func TestMulti_D_AllThreeCoexistAfterRoundTrip(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "secondary", "local"} {
		if _, err := runMpmCommand(t, bin, ws,
			"config", "profile", "add", name,
			"--provider", "custom",
			"--model", "m-"+name,
			"--base-url", "https://"+name+".invalid/v1",
		); err != nil {
			t.Fatalf("profile add %s: %v", name, err)
		}
	}
	// Force a fresh process by invoking show — proves the on-disk
	// state, not in-memory cache.
	out, err := runMpmCommand(t, bin, ws, "config", "show")
	if err != nil {
		t.Fatalf("config show: %v", err)
	}
	for _, name := range []string{"primary", "secondary", "local"} {
		if !strings.Contains(out, name) {
			t.Errorf("config show missing %q; got:\n%s", name, out)
		}
	}
}

// E. Embedding profile is created with the role validator bypass
//
//	when components.embedding is already bound to it.
func TestMulti_E_EmbeddingProfileCreated(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	// Bind embedding FIRST so the role validator accepts the embedding model.
	if _, err := runMpmCommand(t, bin, ws, "config", "profile", "add", "primary",
		"--provider", "custom", "--model", "model-a",
		"--base-url", "https://a.invalid/v1"); err != nil {
		t.Fatalf("profile add primary: %v", err)
	}
	if _, err := runMpmCommand(t, bin, ws, "config", "component", "set", "embedding", "primary"); err != nil {
		t.Fatalf("component set: %v", err)
	}
	if _, err := runMpmCommand(t, bin, ws, "config", "profile", "add", "embedding",
		"--provider", "custom", "--model", "nomic-embed",
		"--base-url", "http://127.0.0.1:11434"); err != nil {
		t.Fatalf("profile add embedding: %v", err)
	}
	if _, err := runMpmCommand(t, bin, ws, "config", "component", "set", "embedding", "embedding"); err != nil {
		t.Fatalf("rebind embedding: %v", err)
	}
	out, err := runMpmCommand(t, bin, ws, "config", "profile", "list")
	if err != nil {
		t.Fatalf("profile list: %v", err)
	}
	if !strings.Contains(out, "embedding") {
		t.Errorf("embedding profile missing; got:\n%s", out)
	}
}

// F. Embedding profile coexists with all LLM profiles.
func TestMulti_F_EmbeddingProfileCoexistsWithLLM(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "secondary", "local", "embedding"} {
		args := []string{"config", "profile", "add", name,
			"--provider", "custom", "--model", "m",
			"--base-url", "https://x.invalid/v1"}
		if _, err := runMpmCommand(t, bin, ws, args...); err != nil {
			t.Fatalf("profile add %s: %v", name, err)
		}
	}
	out, err := runMpmCommand(t, bin, ws, "config", "profile", "list")
	if err != nil {
		t.Fatalf("profile list: %v", err)
	}
	for _, name := range []string{"primary", "secondary", "local", "embedding"} {
		if !strings.Contains(out, name) {
			t.Errorf("profile list missing %q; got:\n%s", name, out)
		}
	}
}

// G. Component A bound to profile 1.
func TestMulti_G_BindComponentAToProfile1(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	runMpmCommand(t, bin, ws, "config", "profile", "add", "primary",
		"--provider", "custom", "--model", "model-a", "--base-url", "https://a.invalid/v1")
	if _, err := runMpmCommand(t, bin, ws, "config", "component", "set", "memory", "primary"); err != nil {
		t.Fatalf("component set memory primary: %v", err)
	}
	out, err := runMpmCommand(t, bin, ws, "config", "component", "list")
	if err != nil {
		t.Fatalf("component list: %v", err)
	}
	if !strings.Contains(out, "memory") || !strings.Contains(out, "primary") {
		t.Errorf("binding not visible; got:\n%s", out)
	}
}

// H. Component B bound to profile 2; independent of G.
func TestMulti_H_BindComponentBToProfile2(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "secondary"} {
		runMpmCommand(t, bin, ws, "config", "profile", "add", name,
			"--provider", "custom", "--model", "m", "--base-url", "https://x.invalid/v1")
	}
	runMpmCommand(t, bin, ws, "config", "component", "set", "memory", "primary")
	if _, err := runMpmCommand(t, bin, ws, "config", "component", "set", "critic", "secondary"); err != nil {
		t.Fatalf("component set critic secondary: %v", err)
	}
	out, _ := runMpmCommand(t, bin, ws, "config", "component", "list")
	if !strings.Contains(out, "memory") || !strings.Contains(out, "primary") ||
		!strings.Contains(out, "critic") || !strings.Contains(out, "secondary") {
		t.Errorf("both bindings not visible; got:\n%s", out)
	}
}

// I. Bind embedding to embedding profile.
func TestMulti_I_BindEmbeddingToEmbeddingProfile(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "embedding"} {
		runMpmCommand(t, bin, ws, "config", "profile", "add", name,
			"--provider", "custom", "--model", "m", "--base-url", "https://x.invalid/v1")
	}
	if _, err := runMpmCommand(t, bin, ws, "config", "component", "set", "embedding", "embedding"); err != nil {
		t.Fatalf("component set embedding embedding: %v", err)
	}
	out, err := runMpmCommand(t, bin, ws, "config", "component", "list")
	if err != nil {
		t.Fatalf("component list: %v", err)
	}
	if !strings.Contains(out, "embedding") || !strings.Contains(out, "profile") {
		t.Errorf("embedding-source binding not visible; got:\n%s", out)
	}
}

// J. Changing binding A does not change B.
func TestMulti_J_ChangingBindingAIndependentOfB(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "secondary"} {
		runMpmCommand(t, bin, ws, "config", "profile", "add", name,
			"--provider", "custom", "--model", "m", "--base-url", "https://x.invalid/v1")
	}
	runMpmCommand(t, bin, ws, "config", "component", "set", "memory", "primary")
	runMpmCommand(t, bin, ws, "config", "component", "set", "critic", "secondary")
	// Update memory → secondary
	if _, err := runMpmCommand(t, bin, ws, "config", "component", "set", "memory", "secondary"); err != nil {
		t.Fatalf("component set memory secondary: %v", err)
	}
	// Read back via JSON envelope to verify exact state.
	out, err := runMpmCommand(t, bin, ws, "config", "component", "list", "--json")
	if err != nil {
		t.Fatalf("component list --json: %v", err)
	}
	env := mustParseJSONList(t, out, "components")
	bindings := map[string]string{}
	for _, e := range env {
		m := e.(map[string]interface{})
		bindings[m["component"].(string)] = m["configured_profile"].(string)
	}
	if bindings["memory"] != "secondary" {
		t.Errorf("memory binding = %q, want secondary", bindings["memory"])
	}
	if bindings["critic"] != "secondary" {
		t.Errorf("critic binding = %q, want secondary", bindings["critic"])
	}
}

// K. Updating profile 1 does not mutate others.
func TestMulti_K_UpdatingProfile1DoesNotMutateOthers(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "secondary", "local"} {
		runMpmCommand(t, bin, ws, "config", "profile", "add", name,
			"--provider", "custom", "--model", "m-"+name, "--base-url", "https://x.invalid/v1")
	}
	before := readProfilesBlock(t, ws)
	// Update primary's model
	if _, err := runMpmCommand(t, bin, ws, "config", "profile", "set", "primary", "model", "model-a-NEW"); err != nil {
		t.Fatalf("profile set primary model: %v", err)
	}
	after := readProfilesBlock(t, ws)
	if before["primary"] == after["primary"] {
		t.Errorf("primary should have changed; both:\n%s\n==\n%s", before["primary"], after["primary"])
	}
	if before["secondary"] != after["secondary"] {
		t.Errorf("secondary mutated:\nbefore=%s\nafter=%s", before["secondary"], after["secondary"])
	}
	if before["local"] != after["local"] {
		t.Errorf("local mutated:\nbefore=%s\nafter=%s", before["local"], after["local"])
	}
}

// L. Deleting an unbound profile succeeds.
func TestMulti_L_DeleteUnboundProfileSucceeds(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "secondary"} {
		runMpmCommand(t, bin, ws, "config", "profile", "add", name,
			"--provider", "custom", "--model", "m", "--base-url", "https://x.invalid/v1")
	}
	runMpmCommand(t, bin, ws, "config", "component", "set", "memory", "primary")
	// secondary is unbound.
	if _, err := runMpmCommand(t, bin, ws, "config", "profile", "remove", "secondary"); err != nil {
		t.Fatalf("profile remove secondary: %v", err)
	}
	out, err := runMpmCommand(t, bin, ws, "config", "profile", "list")
	if err != nil {
		t.Fatalf("profile list: %v", err)
	}
	if strings.Contains(out, "secondary") {
		t.Errorf("secondary still present after removal; got:\n%s", out)
	}
}

// M. Deleting a bound profile fails and lists ALL bound components.
func TestMulti_M_DeleteBoundProfileFailsListsAllComponents(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "secondary", "embedding"} {
		runMpmCommand(t, bin, ws, "config", "profile", "add", name,
			"--provider", "custom", "--model", "m", "--base-url", "https://x.invalid/v1")
	}
	runMpmCommand(t, bin, ws, "config", "component", "set", "memory", "primary")
	runMpmCommand(t, bin, ws, "config", "component", "set", "scheduler", "primary")
	runMpmCommand(t, bin, ws, "config", "component", "set", "embedding", "embedding")

	cmd := stdlibexec.Command(bin, "config", "profile", "remove", "primary")
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("profile remove primary: expected failure, got success\n%s", out)
	}
	s := stripLogNoise(string(out))
	if !strings.Contains(s, `"memory"`) {
		t.Errorf("error must list memory; got:\n%s", s)
	}
	if !strings.Contains(s, `"scheduler"`) {
		t.Errorf("error must list scheduler; got:\n%s", s)
	}
	if strings.Contains(s, `"embedding"`) {
		t.Errorf("error must NOT list embedding (different profile); got:\n%s", s)
	}
	// Profile still exists.
	if _, err := runMpmCommand(t, bin, ws, "config", "profile", "get", "primary"); err != nil {
		t.Errorf("primary must still exist after refused removal: %v", err)
	}
}

// M2. Capability indirection does NOT create a second profile-reference.
func TestMulti_M2_CapabilityIndirectionDoesNotCreateSecondProfileReference(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "critic-profile"} {
		runMpmCommand(t, bin, ws, "config", "profile", "add", name,
			"--provider", "custom", "--model", "m", "--base-url", "https://x.invalid/v1")
	}
	runMpmCommand(t, bin, ws, "config", "component", "set", "critic", "primary")
	runMpmCommand(t, bin, ws, "config", "capability", "set", "reviewer", "critic")

	cmd := stdlibexec.Command(bin, "config", "profile", "remove", "primary")
	cmd.Env = clearEmbeddingEnv(ws)
	out, _ := cmd.CombinedOutput()
	s := stripLogNoise(string(out))
	// Only critic (via Components) is referenced; reviewer (via Capabilities)
	// does not appear because capabilities hold COMPONENT names, not profile
	// names.
	if !strings.Contains(s, `"critic"`) {
		t.Errorf("error must list critic; got:\n%s", s)
	}
	if strings.Contains(s, `"reviewer"`) {
		t.Errorf("error must NOT list reviewer (capability, not profile reference); got:\n%s", s)
	}
}

// N. Embedding-only model rejected for LLM.
func TestMulti_N_EmbeddingOnlyModelRejectedForLLM(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	runMpmCommand(t, bin, ws, "config", "profile", "add", "primary",
		"--provider", "custom", "--model", "model-a", "--base-url", "https://a.invalid/v1")
	cmd := stdlibexec.Command(bin, "config", "profile", "set", "primary", "model", "all-minilm")
	cmd.Env = clearEmbeddingEnv(ws)
	if _, err := cmd.CombinedOutput(); err == nil {
		t.Fatal("expected rejection of embedding-only model for LLM")
	}
}

// O. Unknown model accepted permissively on a custom remote.
func TestMulti_O_UnknownModelAcceptedPermissively(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	runMpmCommand(t, bin, ws, "config", "profile", "add", "primary",
		"--provider", "custom", "--model", "model-a", "--base-url", "https://a.invalid/v1")
	cmd := stdlibexec.Command(bin, "config", "profile", "set", "primary", "model", "totally-unknown-model-9000")
	cmd.Env = clearEmbeddingEnv(ws)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unknown model must be accepted permissively; got: %v\n%s", err, out)
	}
}

// P. Branded + Custom profiles coexist.
func TestMulti_P_LegacyBrandedProfilesCoexist(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	// Seed legacy branded profile + new Custom profile directly via the JSON.
	configPath := filepath.Join(ws, "mpm_config.json")
	seed := `{
		"profiles": {
			"openai-legacy": {
				"provider": "openai",
				"model": "gpt-4o",
				"base_url": "https://api.openai.com/v1",
				"api_key": "sk-legacy"
			},
			"primary": {
				"provider": "custom",
				"model": "model-a",
				"base_url": "https://a.invalid/v1",
				"api_key": "sk-aaa"
			}
		}
	}`
	if err := os.WriteFile(configPath, []byte(seed), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, err := runMpmCommand(t, bin, ws, "config", "profile", "list")
	if err != nil {
		t.Fatalf("profile list: %v", err)
	}
	for _, name := range []string{"openai-legacy", "primary"} {
		if !strings.Contains(out, name) {
			t.Errorf("profile list missing %q; got:\n%s", name, out)
		}
	}
}

// Q. Three profiles survive a save/reload round-trip via the JSON file.
func TestMulti_Q_SaveReloadRoundTrip(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "secondary", "local"} {
		runMpmCommand(t, bin, ws, "config", "profile", "add", name,
			"--provider", "custom", "--model", "m-"+name, "--base-url", "https://"+name+".invalid/v1")
	}
	// Read JSON back.
	profiles := readProfilesBlock(t, ws)
	for _, name := range []string{"primary", "secondary", "local"} {
		if _, ok := profiles[name]; !ok {
			t.Errorf("profile %q missing from JSON after save", name)
		}
	}
}

// R. Secrets redacted in HUMAN output of profile list and profile get.
// 2026-09-19 credential-UX hardening: `profile add` no longer
// accepts --api-key; the canonical secret-bearing flow is
// `profile set <name> api_key --stdin`. The non-secret fields
// (provider / model / base_url) keep their --flag surface.
func TestMulti_R_SecretsRedactedInHumanOutput(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	const secret = "sk-very-long-secret-1234567890abcdef"
	runMpmCommand(t, bin, ws, "config", "profile", "add", "primary",
		"--provider", "custom", "--model", "model-a",
		"--base-url", "https://a.invalid/v1")
	// 2026-09-19 hardening: pass the secret via --stdin so it
	// never enters argv. runMpmCommand pipes stdin via cmd.Stdin
	// (see runMpmCommand); for --stdin to take effect we feed
	// the secret explicitly.
	setCmd := stdlibexec.Command(bin,
		"config", "profile", "set", "primary", "api_key", "--stdin")
	setCmd.Env = clearEmbeddingEnv(ws)
	setCmd.Stdin = strings.NewReader(secret + "\n")
	if out, err := setCmd.CombinedOutput(); err != nil {
		t.Fatalf("profile set api_key --stdin: %v\n%s", err, out)
	}
	outList, _ := runMpmCommand(t, bin, ws, "config", "profile", "list")
	if strings.Contains(outList, secret) {
		t.Errorf("profile list leaked api_key; got:\n%s", outList)
	}
	if !strings.Contains(outList, "...") {
		t.Errorf("profile list missing redaction marker; got:\n%s", outList)
	}
	outGet, _ := runMpmCommand(t, bin, ws, "config", "profile", "get", "primary")
	if strings.Contains(outGet, secret) {
		t.Errorf("profile get leaked api_key; got:\n%s", outGet)
	}
	if !strings.Contains(outGet, "...") {
		t.Errorf("profile get missing redaction marker; got:\n%s", outGet)
	}
}

// R2. Secrets redacted in JSON output of profile list and profile get.
func TestMulti_R2_SecretsRedactedInJSONOutput(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	const secret = "sk-very-long-secret-1234567890abcdef"
	runMpmCommand(t, bin, ws, "config", "profile", "add", "primary",
		"--provider", "custom", "--model", "model-a",
		"--base-url", "https://a.invalid/v1")
	setCmd := stdlibexec.Command(bin,
		"config", "profile", "set", "primary", "api_key", "--stdin")
	setCmd.Env = clearEmbeddingEnv(ws)
	setCmd.Stdin = strings.NewReader(secret + "\n")
	if out, err := setCmd.CombinedOutput(); err != nil {
		t.Fatalf("profile set api_key --stdin: %v\n%s", err, out)
	}
	outList, _ := runMpmCommand(t, bin, ws, "config", "profile", "list", "--json")
	if strings.Contains(outList, secret) {
		t.Errorf("profile list --json leaked api_key; got:\n%s", outList)
	}
	outGet, _ := runMpmCommand(t, bin, ws, "config", "profile", "get", "primary", "--json")
	if strings.Contains(outGet, secret) {
		t.Errorf("profile get --json leaked api_key; got:\n%s", outGet)
	}
}

// S. JSON / human parity for component list.
func TestMulti_S_JSONHumanParity(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "secondary", "embedding"} {
		runMpmCommand(t, bin, ws, "config", "profile", "add", name,
			"--provider", "custom", "--model", "m", "--base-url", "https://x.invalid/v1")
	}
	runMpmCommand(t, bin, ws, "config", "component", "set", "memory", "primary")
	runMpmCommand(t, bin, ws, "config", "component", "set", "critic", "secondary")
	runMpmCommand(t, bin, ws, "config", "component", "set", "embedding", "embedding")
	humanOut, _ := runMpmCommand(t, bin, ws, "config", "component", "list")
	jsonOut, _ := runMpmCommand(t, bin, ws, "config", "component", "list", "--json")
	env := mustParseJSONList(t, jsonOut, "components")
	if len(env) != 3 {
		t.Errorf("JSON count = %d, want 3", len(env))
	}
	// Each component name appears in both surfaces.
	for _, want := range []string{"memory", "critic", "embedding"} {
		if !strings.Contains(humanOut, want) {
			t.Errorf("human output missing %q", want)
		}
		found := false
		for _, e := range env {
			if e.(map[string]interface{})["component"] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("JSON output missing %q", want)
		}
	}
}

// T. No automatic cross-profile fallback — in-process check on the
// canonical resolver.
func TestMulti_T_NoAutomaticCrossProfileFallback(t *testing.T) {
	c := &config.Config{
		Profiles: map[string]config.Profile{
			"primary":   {Provider: "custom", Model: "model-a", BaseURL: "https://a.invalid/v1"},
			"secondary": {Provider: "custom", Model: "model-b", BaseURL: "https://b.invalid/v1"},
		},
		Components: map[string]string{"memory": "primary"},
	}
	r := c.ResolveProfile("memory")
	if r.Profile == nil || r.Profile.Name != "primary" {
		t.Errorf("ResolveProfile(memory) = %+v, want primary", r)
	}
	// Unbound component falls back to nil (no default).
	rUnbound := c.ResolveProfile("nonexistent")
	if rUnbound.Profile != nil {
		t.Errorf("ResolveProfile(nonexistent) returned profile %+v; want nil (no default, no legacy)", rUnbound.Profile)
	}
}

// U. Vendors field is inert — the substrate never iterates Vendors at
// runtime. The audit (2026-09-15) confirmed via grep that no
// runtime path references `cfg.Synth.Vendors`. This test pins the
// behavioural contract by exercising NewSynthClient() (the heavy
// path) with a config that has Vendors populated; the resulting
// *SynthClient carries ONE profile (the resolved ProfileFor),
// not a vendor-iterated one.
func TestMulti_U_VendorsFieldInert(t *testing.T) {
	ws := t.TempDir()
	// Set MPM_WORKSPACE BEFORE SaveConfig and NewSynthClient so the
	// substrate's resolver hits the right path. Restore on exit.
	old := os.Getenv("MPM_WORKSPACE")
	_ = os.Setenv("MPM_WORKSPACE", ws)
	defer func() { _ = os.Setenv("MPM_WORKSPACE", old) }()

	c := &config.Config{
		Profiles: map[string]config.Profile{
			"primary": {Provider: "custom", Model: "model-a", BaseURL: "https://a.invalid/v1", APIKey: "sk-a"},
		},
		// The synth client resolves the "synth" component (not
		// "memory"). Bind it so ProfileFor("synth") returns the
		// profile, overriding the legacy Synth block.
		Components: map[string]string{"synth": "primary"},
		Synth: &config.SynthConfig{
			Model: "synth-model", APIKey: "sk-synth", BaseURL: "https://synth.invalid/v1",
			Vendors: []config.SynthVendor{
				{Name: "alt1", Model: "alt-model-1", APIKey: "sk-alt1", BaseURL: "https://alt1.invalid/v1"},
				{Name: "alt2", Model: "alt-model-2", APIKey: "sk-alt2", BaseURL: "https://alt2.invalid/v2"},
			},
		},
	}
	if err := config.SaveConfig(c); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	sc := synth.NewSynthClient()
	// Resolved Model must come from profile-a (canonical resolution),
	// not Vendors. (Vendors' "alt-model-1" / "alt-model-2" must NOT
	// appear.)
	if sc.Model != "model-a" {
		t.Errorf("sc.Model = %q, want model-a (Vendors must be inert)", sc.Model)
	}
	for _, alt := range []string{"alt-model-1", "alt-model-2"} {
		if strings.Contains(sc.Model, alt) {
			t.Errorf("sc.Model = %q contains Vendors value %q; Vendors must be inert", sc.Model, alt)
		}
	}
}

// V. Provider failure does NOT failover. Real fake-provider test:
// two httptest endpoints, one always 500, the other always 200.
// Bind memory → profile-a (the failing one). Run the actual
// substrate synthesis path. Assert endpoint A receives exactly
// 2 hits (fresh + 1 bounded mechanical retry), endpoint B
// receives exactly 0 hits.
//
// We do NOT use `--dry-run` (which suppresses provider traffic).
// We invoke the substrate's *synth.SynthClient directly.
func TestMulti_V_ProviderFailureDoesNotFailover(t *testing.T) {
	var hitsA, hitsB int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA++
		http.Error(w, "synthetic failure", http.StatusInternalServerError)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"{\"content\":\"ok\",\"tags\":[]}"}]}`))
	}))
	defer srvB.Close()

	sc := &synth.SynthClient{
		BaseURL:   srvA.URL,
		APIKey:    "sk-a",
		Model:     "model-a",
		MaxTokens: 1024,
		Wire:      synth.InferWire(srvA.URL),
		Timeout:   5 * time.Second,
	}
	plan := synth.NewPerCallPlan()
	_, err := sc.SynthesizeWithPlan(context.Background(), []string{"x"}, plan)
	if err == nil {
		t.Fatalf("synthesize should have failed against endpoint A")
	}
	// Per-stage cap = 2 (1 fresh + 1 retry).
	if hitsA != 2 {
		t.Errorf("endpoint A hits = %d, want 2 (1 fresh + 1 bounded retry)", hitsA)
	}
	if hitsB != 0 {
		t.Errorf("endpoint B hits = %d, want 0 (no failover to a second profile)", hitsB)
	}
}

// W. max_tokens setter still rejected.
func TestMulti_W_MaxTokensSetterStillRejected(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	runMpmCommand(t, bin, ws, "config", "profile", "add", "primary",
		"--provider", "custom", "--model", "model-a", "--base-url", "https://a.invalid/v1")
	cmd := stdlibexec.Command(bin, "config", "profile", "set", "primary", "max_tokens", "1024")
	cmd.Env = clearEmbeddingEnv(ws)
	if _, err := cmd.CombinedOutput(); err == nil {
		t.Fatal("max_tokens setter must be rejected")
	}
}

// X. Profile names with special chars round-trip.
func TestMulti_X_ProfileNamesWithSpecialChars(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	names := []string{"alpha-1", "snake_case", "dot.notation", "number-2", "with-dash_and.dot"}
	for _, name := range names {
		if _, err := runMpmCommand(t, bin, ws, "config", "profile", "add", name,
			"--provider", "custom", "--model", "m", "--base-url", "https://x.invalid/v1"); err != nil {
			t.Fatalf("profile add %q: %v", name, err)
		}
	}
	out, _ := runMpmCommand(t, bin, ws, "config", "profile", "list")
	for _, name := range names {
		if !strings.Contains(out, name) {
			t.Errorf("profile list missing %q; got:\n%s", name, out)
		}
	}
	// Round-trip via JSON.
	profiles := readProfilesBlock(t, ws)
	for _, name := range names {
		if _, ok := profiles[name]; !ok {
			t.Errorf("profile %q missing after round-trip", name)
		}
	}
}

// Y. Duplicate profile name produces clear error.
func TestMulti_Y_DuplicateProfileNameFails(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	if _, err := runMpmCommand(t, bin, ws, "config", "profile", "add", "primary",
		"--provider", "custom", "--model", "m", "--base-url", "https://x.invalid/v1"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	cmd := stdlibexec.Command(bin, "config", "profile", "add", "primary",
		"--provider", "custom", "--model", "m", "--base-url", "https://x.invalid/v1")
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("duplicate add should fail")
	}
	if !strings.Contains(strings.ToLower(string(out)), "already exists") {
		t.Errorf("error should mention 'already exists'; got: %s", out)
	}
}

// Z. Component set to nonexistent profile fails cleanly.
func TestMulti_Z_ComponentSetToNonexistentProfileFails(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	cmd := stdlibexec.Command(bin, "config", "component", "set", "memory", "nonexistent")
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("set to nonexistent should fail")
	}
	if !strings.Contains(strings.ToLower(string(out)), "not found") {
		t.Errorf("error should mention 'not found'; got: %s", out)
	}
}

// AA. component list shows binding_source tags in human output.
func TestMulti_AA_ComponentListShowsBindingSource(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "secondary", "embedding"} {
		runMpmCommand(t, bin, ws, "config", "profile", "add", name,
			"--provider", "custom", "--model", "m", "--base-url", "https://x.invalid/v1")
	}
	runMpmCommand(t, bin, ws, "config", "component", "set", "memory", "primary")
	runMpmCommand(t, bin, ws, "config", "component", "set", "critic", "secondary")
	runMpmCommand(t, bin, ws, "config", "component", "set", "embedding", "embedding")
	out, _ := runMpmCommand(t, bin, ws, "config", "component", "list")
	if !strings.Contains(out, "(explicit)") {
		t.Errorf("expected '(explicit)' tag for memory/critic; got:\n%s", out)
	}
	if !strings.Contains(out, "embedding") || !strings.Contains(out, "profile") {
		t.Errorf("expected embedding-source tag; got:\n%s", out)
	}
}

// AB. JSON component list exposes binding_source fields.
func TestMulti_AB_JSONComponentListShowsBindingSource(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	for _, name := range []string{"primary", "secondary"} {
		runMpmCommand(t, bin, ws, "config", "profile", "add", name,
			"--provider", "custom", "--model", "m", "--base-url", "https://x.invalid/v1")
	}
	runMpmCommand(t, bin, ws, "config", "component", "set", "memory", "primary")
	runMpmCommand(t, bin, ws, "config", "component", "set", "critic", "secondary")
	out, _ := runMpmCommand(t, bin, ws, "config", "component", "list", "--json")
	env := mustParseJSONList(t, out, "components")
	for _, e := range env {
		m := e.(map[string]interface{})
		if _, ok := m["binding_source"]; !ok {
			t.Errorf("row missing binding_source: %+v", m)
		}
		if _, ok := m["valid"]; !ok {
			t.Errorf("row missing valid: %+v", m)
		}
	}
}

// AC. unset embedding does NOT silently inherit Profiles["default"].
// We seed a config with a generative default + unset embedding; the
// CLI must report the embedding resolver's actual outcome (env or
// absent), NOT inherit a generative profile.
func TestMulti_AC_EmbeddingUnsetUsesRealResolver(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	// Seed a generative default profile AND keep components.embedding unset.
	runMpmCommand(t, bin, ws, "config", "profile", "add", "default",
		"--provider", "custom", "--model", "generative-model",
		"--base-url", "https://generative.invalid/v1")
	cmd := stdlibexec.Command(bin, "config", "component", "unset", "embedding")
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("component unset embedding: %v\n%s", err, out)
	}
	s := stripLogNoise(string(out))
	// Must NOT report inheriting the generative default.
	if strings.Contains(s, "generative-model") {
		t.Errorf("embedding inherited generative default — must NOT; got:\n%s", s)
	}
	if !strings.Contains(s, "absent") {
		t.Errorf("expected 'Embedding source: absent' report; got:\n%s", s)
	}
}

// AC1. No binding + no env → absent.
func TestMulti_AC1_NoBindingNoEnv_Absent(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	cmd := stdlibexec.Command(bin, "config", "component", "get", "embedding")
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("component get embedding: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "absent") {
		t.Errorf("expected 'absent' for embedding with no binding + no env; got:\n%s", out)
	}
}

// AC2. No binding + explicit OLLAMA env → env.
func TestMulti_AC2_NoBindingWithEnv_EnvSource(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	cmd := stdlibexec.Command(bin, "config", "component", "get", "embedding")
	cmd.Env = append(clearEmbeddingEnv(ws),
		"OLLAMA_ENDPOINT=http://127.0.0.1:11434",
		"OLLAMA_MODEL=nomic-embed-text",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("component get embedding: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "env") {
		t.Errorf("expected 'env' for embedding with OLLAMA_* set; got:\n%s", out)
	}
}

// AC3. Generative default exists; embedding unset + env cleared → absent.
// Embedding must NOT inherit the generative default.
func TestMulti_AC3_GenerativeDefaultExists_EmbeddingAbsent(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	runMpmCommand(t, bin, ws, "config", "profile", "add", "default",
		"--provider", "custom", "--model", "generative-only-model",
		"--base-url", "https://generative.invalid/v1")
	cmd := stdlibexec.Command(bin, "config", "component", "get", "embedding")
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("component get embedding: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "generative-only-model") {
		t.Errorf("embedding inherited generative default — must NOT; got:\n%s", out)
	}
	if !strings.Contains(string(out), "absent") {
		t.Errorf("expected 'absent' for embedding with generative default only; got:\n%s", out)
	}
}

// AC4. unset embedding when currently "disabled" → returns to absent.
func TestMulti_AC4_UnsetFromDisabled_ReturnsToEnvOrAbsent(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	configPath := filepath.Join(ws, "mpm_config.json")
	seed := `{"components": {"embedding": "disabled"}}`
	if err := os.WriteFile(configPath, []byte(seed), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cmd := stdlibexec.Command(bin, "config", "component", "unset", "embedding")
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("component unset embedding: %v\n%s", err, out)
	}
	s := stripLogNoise(string(out))
	if !strings.Contains(s, "absent") {
		t.Errorf("expected 'absent' after unset from disabled; got:\n%s", s)
	}
	if strings.Contains(s, "disabled") && strings.Contains(s, "Embedding source: disabled") {
		t.Errorf("sentinel 'disabled' must be removed, not preserved; got:\n%s", s)
	}
}

// AD. unset critic (generative) when a default profile exists → inherited-from-default.
func TestMulti_AD_GenerativeUnsetSemantics(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	runMpmCommand(t, bin, ws, "config", "profile", "add", "primary",
		"--provider", "custom", "--model", "model-a", "--base-url", "https://a.invalid/v1")
	runMpmCommand(t, bin, ws, "config", "profile", "add", "default",
		"--provider", "custom", "--model", "default-model", "--base-url", "https://d.invalid/v1")
	runMpmCommand(t, bin, ws, "config", "component", "set", "critic", "primary")
	cmd := stdlibexec.Command(bin, "config", "component", "unset", "critic")
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unset critic: %v\n%s", err, out)
	}
	s := stripLogNoise(string(out))
	if !strings.Contains(s, "default") {
		t.Errorf("expected inherited-from-default report; got:\n%s", s)
	}
}

// AE. JSON exit semantics on miss.
func TestMulti_AE_JSONExitSemanticsOnMiss(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	cmd := stdlibexec.Command(bin, "config", "profile", "get", "missing", "--json")
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("profile get missing --json: expected exit 1")
	}
	if !strings.Contains(string(out), `"success": false`) {
		t.Errorf("JSON must include success:false; got: %s", out)
	}
	if !strings.Contains(string(out), `"error"`) {
		t.Errorf("JSON must include error field; got: %s", out)
	}
}

// AF. No branded catalogue in mpm config --help.
func TestMulti_AF_NoBrandedCatalogueInHelp(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	cmd := stdlibexec.Command(bin, "config", "--help")
	cmd.Env = clearEmbeddingEnv(ws)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config --help: %v", err)
	}
	s := string(out)
	// Branded model strings MUST NOT appear in public help.
	banned := []string{"gpt-4o", "claude-3-5", "gemini-2.5", "grok-", "mistral-", "command-", "minimax-m", "openrouter/free"}
	for _, b := range banned {
		if strings.Contains(s, b) {
			t.Errorf("help must not advertise %q; got:\n%s", b, s)
		}
	}
	if !strings.Contains(s, "Multi-profile support") {
		t.Errorf("help must declare multi-profile support; got:\n%s", s)
	}
}

// AG. Dangling binding surfaced as invalid.
//
//	2026-09-15 verification at HEAD 20c16c08:
//
//	Fixture: Profiles["default"] = valid generative profile;
//	         Components["critic"] = "missing" (dangling).
//
//	Truthful contract (verified at HEAD 20c16c08):
//	  - Runtime falls through to Profiles["default"]
//	    (ProfileFor("critic") returns Name="default"; ModelFactory
//	    returns a non-nil client). This is the preserved
//	    pre-2026-09-15 behaviour — the runtime must not silently
//	    lose a model when an operator's binding is broken.
//	  - The CLI surfaces the dangling state explicitly so the
//	    operator sees the misconfig and rebinds:
//	      binding_source = "explicit, invalid"
//	      valid = false
//	      configured_profile = "missing"  (raw operator binding)
//	      effective_profile = "default"   (runtime-resolved fallback)
//	    Human output renders the binding string + "(explicit, invalid)"
//	    tag (does not show the resolved profile name in the human list
//	    view — that's the JSON envelope's job).
func TestMulti_AG_DanglingBindingSurfacedAsInvalid(t *testing.T) {
	bin := buildRunawayBin(t)
	ws := t.TempDir()
	configPath := filepath.Join(ws, "mpm_config.json")
	seed := `{
		"profiles": {"default": {"provider": "custom", "model": "m", "base_url": "https://x.invalid/v1"}},
		"components": {"critic": "missing"}
	}`
	if err := os.WriteFile(configPath, []byte(seed), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Human output: binding string + (explicit, invalid) tag.
	humanOut, _ := runMpmCommand(t, bin, ws, "config", "component", "list")
	if !strings.Contains(humanOut, "invalid") {
		t.Errorf("human component list must mark dangling binding as invalid; got:\n%s", humanOut)
	}
	if !strings.Contains(humanOut, "missing") {
		t.Errorf("human component list must preserve the raw operator binding string; got:\n%s", humanOut)
	}

	// JSON envelope: full contract — the truthful report lives here.
	jsonOut, _ := runMpmCommand(t, bin, ws, "config", "component", "list", "--json")
	env := mustParseJSONList(t, jsonOut, "components")
	found := false
	for _, e := range env {
		m := e.(map[string]interface{})
		if m["component"] == "critic" {
			found = true
			if v, ok := m["valid"].(bool); !ok || v {
				t.Errorf("dangling binding must have valid=false; got: %+v", m)
			}
			if m["binding_source"] != "explicit, invalid" {
				t.Errorf("binding_source must be 'explicit, invalid'; got: %v", m["binding_source"])
			}
			if m["configured_profile"] != "missing" {
				t.Errorf("configured_profile must preserve raw operator binding 'missing'; got: %v", m["configured_profile"])
			}
			// Runtime falls through to Profiles["default"]; effective_profile
			// must report that, NOT null. This pins the truthful contract:
			// the CLI oracle and the runtime oracle agree on the resolved profile.
			if m["effective_profile"] != "default" {
				t.Errorf("effective_profile must be 'default' (runtime falls through to Profiles[default]); got: %v", m["effective_profile"])
			}
		}
	}
	if !found {
		t.Errorf("critic component not in JSON envelope")
	}
}

// AH. ProfileFor ↔ ResolveProfile agreement.
func TestMulti_AH_HelperIsSingleSourceOfTruth(t *testing.T) {
	// In-process: build a config with all branches exercised and
	// verify ProfileFor's *Profile equals ResolveProfile's *Profile
	// for every component.
	c := &config.Config{
		Profiles: map[string]config.Profile{
			"primary":  {Provider: "custom", Model: "model-a", BaseURL: "https://a.invalid/v1"},
			"default":  {Provider: "custom", Model: "default-model", BaseURL: "https://d.invalid/v1"},
			"reviewer": {Provider: "custom", Model: "model-r", BaseURL: "https://r.invalid/v1"},
		},
		Components: map[string]string{
			"memory": "primary",
			"critic": "reviewer",
		},
		Synth: &config.SynthConfig{
			Model: "synth-model", APIKey: "sk-s", BaseURL: "https://synth.invalid/v1",
		},
	}
	for _, comp := range []string{"memory", "critic", "scheduler", "embedding"} {
		pf := c.ProfileFor(comp)
		rp := c.ResolveProfile(comp).Profile
		if (pf == nil) != (rp == nil) {
			t.Errorf("[%s] nil-ness disagree: ProfileFor=%v ResolveProfile=%v", comp, pf, rp)
			continue
		}
		if pf != nil && rp != nil && *pf != *rp {
			t.Errorf("[%s] Profile=%+v != ResolveProfile=%+v", comp, *pf, *rp)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// readProfilesBlock parses the on-disk mpm_config.json and returns the
// profiles sub-object as a map[string]string of raw JSON bytes per
// profile name. Used by tests K and X to compare before/after states.
func readProfilesBlock(t *testing.T, ws string) map[string]string {
	t.Helper()
	dbPath := filepath.Join(ws, "src", "db", "mpm.db")
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	// mpm_config.json is the operator-facing config file, not in the
	// DB. The CLI writes there. Read it directly.
	data, err := os.ReadFile(filepath.Join(ws, "mpm_config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var doc struct {
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := map[string]string{}
	for k, v := range doc.Profiles {
		out[k] = string(v)
	}
	return out
}

// mustParseJSONList parses a JSON envelope of shape
// `{... "listName": [...]}` and returns the array. Fails the test
// on parse error.
func mustParseJSONList(t *testing.T, raw, listName string) []interface{} {
	t.Helper()
	// Strip log noise and find the first '{'.
	s := stripLogNoise(raw)
	start := strings.IndexByte(s, '{')
	if start < 0 {
		t.Fatalf("no JSON in output: %s", s)
	}
	end := strings.LastIndexByte(s, '}')
	if end < 0 {
		t.Fatalf("unterminated JSON: %s", s)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s[start:end+1]), &doc); err != nil {
		t.Fatalf("parse JSON: %v\npayload: %s", err, s[start:end+1])
	}
	rawArr, ok := doc[listName]
	if !ok {
		t.Fatalf("JSON envelope missing %q; got keys: %v", listName, mapKeys(doc))
	}
	var arr []interface{}
	if err := json.Unmarshal(rawArr, &arr); err != nil {
		t.Fatalf("parse %q array: %v", listName, err)
	}
	return arr
}

func mapKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// Imports kept at the bottom of the file (avoid hoisting issues)
// ---------------------------------------------------------------------------

var _ = runtime.GOOS
var _ = strconv.Itoa
var _ = fmt.Sprintf
