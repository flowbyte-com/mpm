// release_pass_20260914_role_validation_test.go — Regression
// coverage for the 2026-09-14 LLM-role validation boundary.
//
// The release-pass brief documents a concrete UX bug: the
// interactive `mpm config` wizard accepted embedding-only
// models (e.g. `all-minilm`) as LLM providers and asked
// generation-specific fields such as Max tokens, producing a
// broken config. The fix routes every public surface that
// can assign an LLM model through `ValidateLLMRole`
// (cmd/mpm/role_validation.go), which uses capability-based
// detection via ProbeOllamaCapabilities
// (internal/core/ollama_capability.go) with a small fallback
// name list for probe-unavailable cases.
//
// All tests use a hermetic workspace via t.TempDir() and a
// fake Ollama HTTP server (httptest) so production state is
// never touched.

package main

import (
	"bytes"
	"encoding/json"
	stdlibexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm/internal/testenv"
)

// TestRoleValidation_RejectsEmbeddingOnlyOllamaLLM is the
// headline regression: when the chosen model is positively
// identified as embedding-only by the runtime, the wizard
// MUST reject it before Max tokens is asked. Pre-fix the
// wizard asked Max tokens anyway and silently produced a
// broken config.
//
// The test stands up a fake Ollama HTTP server that returns
// `capabilities: ["embedding"]` for the embedding model.
// `mpm config profile set default model <embedding-model>`
// (the non-interactive LLM path) must reject.
func TestRoleValidation_RejectsEmbeddingOnlyOllamaLLM(t *testing.T) {
	bin := buildRoleValidationBin(t)
	ws := t.TempDir()
	srv := startFakeOllama(t, fakeOllamaOpts{
		ShowCapabilities: map[string][]string{
			"all-minilm": {"embedding"},
		},
		TagsCapabilities: nil,
	})
	defer srv.Close()

	// Seed the default profile via profile add so we have a
	// target for `profile set model`.
	cmd := stdlibexec.Command(bin, "config", "profile", "add", "default",
		"--provider", "ollama",
		"--base-url", srv.URL+"/api/embed",
	)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed default profile: %v\n%s", err, out)
	}

	// Now try to assign an embedding-only model as the LLM.
	cmd = stdlibexec.Command(bin, "config", "profile", "set", "default",
		"model", "all-minilm")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("assigning embedding-only model as LLM must error; got success. Output:\n%s", out)
	}
	s := stripLogNoise(string(out))
	if !strings.Contains(s, "embedding model, not a generative LLM") {
		t.Fatalf("rejection message must name the embedding/role distinction; got:\n%s", s)
	}
	if !strings.Contains(s, "mpm config profile add") {
		t.Fatalf("rejection must direct operator at the embedding manual config path; got:\n%s", s)
	}

	// Verify the config was NOT mutated. The seeded default
	// profile must still have an empty Model field.
	cmd = stdlibexec.Command(bin, "config", "profile", "list")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	listOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list profiles: %v\n%s", err, listOut)
	}
	if strings.Contains(stripLogNoise(string(listOut)), "all-minilm") {
		t.Fatalf("rejected model must not be persisted; got:\n%s", listOut)
	}
}

// TestRoleValidation_GenerativeOllamaLLMAccepted asserts that
// a model whose capabilities include "completion" is accepted
// by the role validator. Pre-fix this assertion would have
// passed (the wizard always proceeded), but it's still
// important for the negative case below — the validator must
// not falsely reject generative models.
func TestRoleValidation_GenerativeOllamaLLMAccepted(t *testing.T) {
	bin := buildRoleValidationBin(t)
	ws := t.TempDir()
	srv := startFakeOllama(t, fakeOllamaOpts{
		ShowCapabilities: map[string][]string{
			"llama3": {"completion"},
		},
	})
	defer srv.Close()

	cmd := stdlibexec.Command(bin, "config", "profile", "add", "default",
		"--provider", "ollama",
		"--base-url", srv.URL+"/api/embed",
	)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed default: %v\n%s", err, out)
	}
	cmd = stdlibexec.Command(bin, "config", "profile", "set", "default",
		"model", "llama3")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generative model must be accepted; got error: %v\n%s", err, out)
	}
}

// TestRoleValidation_UnknownCapabilityAccepted asserts that
// the validator does NOT falsely reject a model when the
// runtime's capability cannot be determined (probe failed
// AND no fallback match). The brief explicitly preserves
// custom-provider flexibility in this case.
func TestRoleValidation_UnknownCapabilityAccepted(t *testing.T) {
	bin := buildRoleValidationBin(t)
	ws := t.TempDir()
	srv := startFakeOllama(t, fakeOllamaOpts{
		// Empty capabilities — probe returns ModelCapabilities{}
		// (FromAPI=false) → caller must accept.
		ShowCapabilities: map[string][]string{},
	})
	defer srv.Close()

	cmd := stdlibexec.Command(bin, "config", "profile", "add", "default",
		"--provider", "ollama",
		"--base-url", srv.URL+"/api/embed",
	)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed default: %v\n%s", err, out)
	}
	cmd = stdlibexec.Command(bin, "config", "profile", "set", "default",
		"model", "totally-unknown-model-xyz")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unknown model must be accepted (probe-fail + name-not-in-fallback); got error: %v\n%s", err, out)
	}
}

// TestRoleValidation_FallbackNameListUsedWhenProbeFails
// asserts the secondary fallback path: when the probe is
// unavailable (server down), well-known embedding model
// names must still be rejected via the fallback list.
// Pre-fix this assertion failed because the wizard blindly
// accepted every name.
func TestRoleValidation_FallbackNameListUsedWhenProbeFails(t *testing.T) {
	bin := buildRoleValidationBin(t)
	ws := t.TempDir()

	// No fake server — probe will fail (refused connection).
	// Use a known embedding model name.
	cmd := stdlibexec.Command(bin, "config", "profile", "add", "default",
		"--provider", "ollama",
		"--base-url", "http://127.0.0.1:1/api/embed", // unreachable
	)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed default: %v\n%s", err, out)
	}
	cmd = stdlibexec.Command(bin, "config", "profile", "set", "default",
		"model", "nomic-embed-text")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("well-known embedding model must be rejected via fallback list when probe fails; got success. Output:\n%s", out)
	}
	s := stripLogNoise(string(out))
	if !strings.Contains(s, "embedding model, not a generative LLM") {
		t.Fatalf("fallback rejection must surface the same canonical wording; got:\n%s", s)
	}
}

// TestRoleValidation_EmbeddingBoundProfileBypassesGuard
// asserts the embedding-profile bypass: when a profile is
// bound to Components["embedding"], assigning an embedding
// model is allowed (the operator wants an embedding model
// in this profile by design).
func TestRoleValidation_EmbeddingBoundProfileBypassesGuard(t *testing.T) {
	bin := buildRoleValidationBin(t)
	ws := t.TempDir()
	srv := startFakeOllama(t, fakeOllamaOpts{
		ShowCapabilities: map[string][]string{
			"all-minilm": {"embedding"},
		},
	})
	defer srv.Close()

	// Create the embedding-bound profile and bind it.
	cmd := stdlibexec.Command(bin, "config", "profile", "add", "embedding-profile",
		"--provider", "ollama",
		"--base-url", srv.URL+"/api/embed",
	)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed embedding profile: %v\n%s", err, out)
	}
	cmd = stdlibexec.Command(bin, "config", "component", "set", "embedding", "embedding-profile")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bind embedding component: %v\n%s", err, out)
	}
	// Now assigning an embedding-only model to the
	// embedding-bound profile MUST succeed (bypass).
	cmd = stdlibexec.Command(bin, "config", "profile", "set", "embedding-profile",
		"model", "all-minilm")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("embedding-bound profile must accept embedding models; got: %v\n%s", err, out)
	}
}

// TestRoleValidation_NoPartialConfigOnRejection asserts the
// brief's invariant F: failed role validation leaves no
// partial mutation. We assert by setting a model, attempting
// to overwrite it with a rejected embedding-only model, then
// re-reading — the originally-set model must still be present.
func TestRoleValidation_NoPartialConfigOnRejection(t *testing.T) {
	bin := buildRoleValidationBin(t)
	ws := t.TempDir()
	srv := startFakeOllama(t, fakeOllamaOpts{
		ShowCapabilities: map[string][]string{
			"all-minilm": {"embedding"},
		},
	})
	defer srv.Close()

	cmd := stdlibexec.Command(bin, "config", "profile", "add", "default",
		"--provider", "ollama",
		"--base-url", srv.URL+"/api/embed",
	)
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed default: %v\n%s", err, out)
	}
	cmd = stdlibexec.Command(bin, "config", "profile", "set", "default",
		"model", "llama3")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed model: %v\n%s", err, out)
	}
	// Attempt rejected overwrite.
	cmd = stdlibexec.Command(bin, "config", "profile", "set", "default",
		"model", "all-minilm")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if _, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("rejected assignment must error")
	}
	// Re-read the model; the originally-set value must still
	// be present (no partial mutation).
	cmd = stdlibexec.Command(bin, "config", "profile", "list")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	listOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list: %v\n%s", err, listOut)
	}
	s := stripLogNoise(string(listOut))
	if !strings.Contains(s, "llama3") {
		t.Fatalf("originally-set model must remain after rejection; got:\n%s", s)
	}
	if strings.Contains(s, "all-minilm") {
		t.Fatalf("rejected model must not appear in the config; got:\n%s", s)
	}
}

// TestRoleValidation_EmbeddingConfigPathUnchanged asserts
// the brief's invariant: the embedding manual configuration
// path must NOT be guarded against embedding-only models.
// 2026-09-14 final-simplification: detect-embedding is
// retired from the public CLI; embedding configuration is
// the manual Custom + protocol path. We pin the contract
// here: the embedding helper text does NOT reject
// embedding-only models, the manual profile add accepts an
// embedding model verbatim, and the `mpm config` help text
// surfaces the new contract.
func TestRoleValidation_EmbeddingConfigPathUnchanged(t *testing.T) {
	bin := buildRoleValidationBin(t)

	// `mpm config --help` must surface both LLM and embedding
	// manual-only sections; neither must reference the retired
	// detect-embedding command.
	// Isolation (2026-09-30): this was `[]string{"PATH=" + …}`, which
	// pinned no workspace, so the child could resolve $HOME/.mpm (a
	// symlink to this repository on a developer machine) or its own CWD.
	// The help text under assertion is static, so pinning the workspace
	// does not change what this test observes.
	cmd := stdlibexec.Command(bin, "config", "--help")
	cmd.Env, cmd.Dir = testenv.Env(t), testenv.Workspace(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config --help: %v\n%s", err, out)
	}
	s := stripLogNoise(string(out))
	if strings.Contains(s, "detect-embedding") {
		t.Fatalf("config --help must NOT reference the retired detect-embedding command; got:\n%s", s)
	}
	if !strings.Contains(s, "Embedding model") {
		t.Fatalf("config --help must surface the embedding manual section; got:\n%s", s)
	}

	// Manual profile creation with an embedding-only model
	// must succeed (the role validator is bypassed for the
	// components["embedding"] binding).
	ws := t.TempDir()
	cmd = stdlibexec.Command(bin, "config", "profile", "add", "embedding")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("profile add embedding: %v\n%s", err, out)
	}
	// Bind components.embedding FIRST so the role validator
	// bypasses the embedding-only guard for this profile.
	cmd = stdlibexec.Command(bin, "config", "component", "set", "embedding", "embedding")
	cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("component set: %v\n%s", err, out)
	}
	for _, kv := range [][2]string{
		{"provider", "custom"},
		{"base_url", "http://127.0.0.1:11434"},
		{"model", "all-minilm"},
	} {
		cmd := stdlibexec.Command(bin, "config", "profile", "set", "embedding", kv[0], kv[1])
		cmd.Env = []string{"MPM_WORKSPACE=" + ws, "PATH=" + lookupTestPath()}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("profile set %s=%s on embedding profile must succeed; got: %v\n%s",
				kv[0], kv[1], err, out)
		}
	}
}

// TestRoleValidation_UnitProbeAndDecision pins the unit-level
// capabilities of the probe and validator helpers. Fast,
// in-process — does not stand up an HTTP server.
func TestRoleValidation_UnitProbeAndDecision(t *testing.T) {
	if got := ValidateLLMRole("ollama", "", ""); got != RoleValid {
		t.Errorf("empty model must return RoleValid; got %v", got)
	}
	if got := ValidateLLMRole("ollama", "llama3", "http://localhost:11434"); got != RoleUnknown {
		t.Errorf("unknown probe + non-fallback name must return RoleUnknown; got %v", got)
	}
	if got := ValidateLLMRole("ollama", "nomic-embed-text", "http://localhost:11434"); got != RoleEmbeddingOnly {
		t.Errorf("fallback name match must return RoleEmbeddingOnly; got %v", got)
	}
	if got := ValidateLLMRole("custom", "nomic-embed-text", "http://localhost:11434"); got != RoleEmbeddingOnly {
		t.Errorf("custom + Ollama-like URL + fallback name must return RoleEmbeddingOnly; got %v", got)
	}
	if got := ValidateLLMRole("custom", "some-model", "https://api.example.com"); got != RoleUnknown {
		t.Errorf("custom + remote URL + non-fallback name must return RoleUnknown; got %v", got)
	}
}

// --- helpers ---

// buildRoleValidationBin builds a fresh mpm binary in a temp
// dir for the role-validation tests.
func buildRoleValidationBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-test")
	cmd := stdlibexec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// ensure json package is referenced (used by the test helpers
// if expanded in future). Cheap compile-time guard.
var _ = json.Marshal
var _ = bytes.NewReader
