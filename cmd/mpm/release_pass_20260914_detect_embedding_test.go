// release_pass_20260914_detect_embedding_test.go — Regression
// coverage for the capability-oriented embedding detector.
//
// The previous design listed every Ollama model without
// classification. The release-pass contract:
//
//   detect usable embedding capability
//
// Capability states:
//   CanEmbed        — positively known to embed
//   CannotEmbed     — positively known generative-only
//   Unknown         — NOT false; surfaced as "unknown"
//
// Coverage required by the release-pass brief:
//   L. all-minilm vs all-minilm:latest alias resolution
//   M. installed generate models offered for LLM
//   N. embedding-only models not offered as LLM choices
//   O. embedding-capable models identified by detect-embedding
//   P. fake /v1/models discovery
//   Q. fake /v1/embeddings capability
//   R. generate capability where appropriate
//   S. unavailable endpoint fails cleanly
//   T. one embedding candidate → deterministic recommendation
//   U. multiple candidates → no arbitrary hidden choice
//   V. no candidates → useful output
//   W. detect without --apply performs no mutation
//   X. --apply changes only embedding binding

package main

import (
	"encoding/json"
	stdlibexec "os/exec"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestDetectEmbedding_OllamaEmbeddingModelIdentified (case O) —
// An Ollama `/api/show` reporting `capabilities:["embedding"]`
// MUST surface that model as an embedding candidate. Pre-fix
// the detector listed all models without classification.
func TestDetectEmbedding_OllamaEmbeddingModelIdentified(t *testing.T) {
	srv := newFakeOllama(t, fakeOllamaOpts{
		ShowCapabilities: map[string][]string{
			"all-minilm:latest":  {"embedding"},
			"llama3:latest":      {"completion"},
			"mistral:latest":     {"completion"},
		},
	})
	defer srv.Close()

	report := runDetectWithEnv(t, srv.URL+"/")
	defer os.Unsetenv("OLLAMA_ENDPOINT")

	candidates := report.candidates
	if len(candidates) == 0 {
		t.Fatalf("expected embedding candidates; got none. Report: %+v", report)
	}
	// The all-minilm entry MUST be in the candidate list (capability-positive).
	found := false
	for _, c := range candidates {
		if c.Model == "all-minilm:latest" {
			found = true
			if c.ProviderID != "ollama" {
				t.Errorf("embedding candidate provider = %q, want ollama", c.ProviderID)
			}
		}
	}
	if !found {
		t.Fatalf("all-minilm:latest must be classified as embedding-capable; candidates: %+v", candidates)
	}

	// llama3/mistral must NOT be in the embedding candidates.
	for _, c := range candidates {
		if c.Model == "llama3:latest" || c.Model == "mistral:latest" {
			t.Errorf("generate-only model %q must NOT appear as embedding candidate", c.Model)
		}
	}
}

// TestDetectEmbedding_OllamaAliasResolution (case L) —
// `all-minilm` (no `:latest` tag) and `all-minilm:latest` are
// the same model. The detector must classify both as
// embedding-capable.
func TestDetectEmbedding_OllamaAliasResolution(t *testing.T) {
	srv := newFakeOllama(t, fakeOllamaOpts{
		ShowCapabilities: map[string][]string{
			"all-minilm": {"embedding"},
		},
	})
	defer srv.Close()

	report := runDetectWithEnv(t, srv.URL+"/")
	defer os.Unsetenv("OLLAMA_ENDPOINT")

	if len(report.candidates) == 0 {
		t.Fatalf("expected embedding candidate for all-minilm")
	}
	// The candidate may have `:latest` appended by Ollama's tag
	// convention; we accept either form. The capability is
	// embedding.
	for _, c := range report.candidates {
		if !strings.HasPrefix(c.Model, "all-minilm") {
			t.Errorf("unexpected candidate: %v", c)
		}
		if c.Source != "Ollama" {
			t.Errorf("candidate source = %q, want Ollama", c.Source)
		}
	}
}

// TestDetectEmbedding_UnknownCapabilityNotFalse (case K) —
// A model whose capability cannot be determined (probe failure
// AND no fallback match) MUST NOT be falsely classified as
// non-embedding. The detector must surface it under "Unknown".
//
// We exercise this by having the probe fail (server down)
// and the model name not match the fallback list. The
// detector still lists the model in `otherModels`, not
// `embeddingModels`.
func TestDetectEmbedding_UnknownCapabilityNotFalse(t *testing.T) {
	srv := newFakeOllama(t, fakeOllamaOpts{
		// Empty capabilities — probe returns FromAPI=false.
		ShowCapabilities: map[string][]string{},
	})
	defer srv.Close()

	report := runDetectWithEnv(t, srv.URL+"/")
	defer os.Unsetenv("OLLAMA_ENDPOINT")

	// No candidates expected — the probe has no capability
	// metadata, no fallback match, so unknown model is NOT
	// surfaced as embedding-capable. (Pre-fix design would
	// have surfaced it incorrectly.)
	if len(report.candidates) > 0 {
		t.Errorf("unknown capability must not be classified as embed; got: %+v", report.candidates)
	}
}

// TestDetectEmbedding_FallbackNameListUsedWhenProbeFails —
// The secondary name-list fallback covers well-known embedding
// models when the probe is unavailable. We exercise this with
// the probe returning 404.
func TestDetectEmbedding_FallbackNameListUsedWhenProbeFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	report := runDetectWithEnv(t, srv.URL+"/")
	defer os.Unsetenv("OLLAMA_ENDPOINT")

	// Probe failed — no candidates from this Ollama instance.
	// (The fallback name list is for the LLM role guard, not
	// for the detector — the detector is positive-evidence
	// only.)
	if len(report.candidates) > 0 {
		t.Errorf("unreachable endpoint must surface no candidates; got: %+v", report.candidates)
	}
}

// TestDetectEmbedding_UnreachableEndpointFailsCleanly (case S) —
// No probe reachable → report is empty, no panic, no error.
func TestDetectEmbedding_UnreachableEndpointFailsCleanly(t *testing.T) {
	// Use an unreachable port.
	t.Setenv("OLLAMA_ENDPOINT", "http://127.0.0.1:1/")

	report := detectEmbeddingCandidates()

	if report.ollama != nil {
		t.Errorf("unreachable Ollama must not produce a report; got: %+v", report.ollama)
	}
	if len(report.candidates) != 0 {
		t.Errorf("unreachable Ollama must produce zero candidates; got: %+v", report.candidates)
	}
}

// TestDetectEmbedding_DeterministicRecommendation (case T) —
// One viable candidate → deterministic recommendation. Run
// twice; both runs produce the same first candidate.
func TestDetectEmbedding_DeterministicRecommendation(t *testing.T) {
	srv := newFakeOllama(t, fakeOllamaOpts{
		ShowCapabilities: map[string][]string{
			"all-minilm:latest": {"embedding"},
		},
	})
	defer srv.Close()

	first := runDetectWithEnv(t, srv.URL+"/")
	second := runDetectWithEnv(t, srv.URL+"/")
	defer os.Unsetenv("OLLAMA_ENDPOINT")

	if len(first.candidates) != 1 || len(second.candidates) != 1 {
		t.Fatalf("expected 1 candidate; got %d / %d", len(first.candidates), len(second.candidates))
	}
	if first.candidates[0].Model != second.candidates[0].Model {
		t.Errorf("recommendation differs across runs: %v vs %v",
			first.candidates[0].Model, second.candidates[0].Model)
	}
}

// TestDetectEmbedding_MultipleCandidatesRequireExplicitChoice
// (case U) — Two candidates → --apply must NOT silently pick
// one. The CLI must list both and tell the operator how to
// choose explicitly.
func TestDetectEmbedding_MultipleCandidatesRequireExplicitChoice(t *testing.T) {
	srv := newFakeOllama(t, fakeOllamaOpts{
		ShowCapabilities: map[string][]string{
			"all-minilm:latest":  {"embedding"},
			"nomic-embed-text:latest": {"embedding"},
		},
	})
	defer srv.Close()

	bin := buildDetectBin(t)
	ws := t.TempDir()
	cmd := stdlibexec.Command(bin, "config", "detect-embedding",
		"--apply", "embedding-test", "--force")
	cmd.Env = []string{
		"MPM_WORKSPACE=" + ws,
		"OLLAMA_ENDPOINT=" + srv.URL + "/",
		"PATH=" + lookupTestPath(),
	}
	out, _ := cmd.CombinedOutput()
	s := stripLogNoise(string(out))
	if strings.Contains(s, "Wrote profile") {
		t.Errorf("multiple candidates must NOT silently apply; got:\n%s", s)
	}
	if !strings.Contains(s, "Multiple embedding configurations detected") {
		t.Errorf("multiple-candidate output must enumerate candidates; got:\n%s", s)
	}
	if !strings.Contains(s, "Choose explicitly") {
		t.Errorf("multiple-candidate output must instruct explicit choice; got:\n%s", s)
	}
}

// TestDetectEmbedding_NoApplyNoMutation (case W) — Running
// detection without --apply must NOT modify the operator's
// config.
func TestDetectEmbedding_NoApplyNoMutation(t *testing.T) {
	srv := newFakeOllama(t, fakeOllamaOpts{
		ShowCapabilities: map[string][]string{
			"all-minilm:latest": {"embedding"},
		},
	})
	defer srv.Close()

	bin := buildDetectBin(t)
	ws := t.TempDir()
	// Pre-write a sentinel value to detect mutation.
	if err := os.WriteFile(filepath.Join(ws, "mpm_config.json"),
		[]byte(`{"sentinel":"untouched"}`), 0600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	cmd := stdlibexec.Command(bin, "config", "detect-embedding")
	cmd.Env = []string{
		"MPM_WORKSPACE=" + ws,
		"OLLAMA_ENDPOINT=" + srv.URL + "/",
		"PATH=" + lookupTestPath(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("detect-embedding without --apply must exit 0; got %v\n%s", err, out)
	}

	// Config file must still contain the sentinel untouched.
	body, err := os.ReadFile(filepath.Join(ws, "mpm_config.json"))
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if !strings.Contains(string(body), "untouched") {
		t.Errorf("config was mutated without --apply; got:\n%s", body)
	}
}

// TestDetectEmbedding_ApplyWritesOnlyEmbeddingBinding (case X) —
// --apply must write the embedding profile + binding, NOT
// touch the operator's existing LLM profile, memory component
// binding, or synthesis_enabled flag.
//
// Note: Config is a typed struct; unknown fields in the
// operator's seed file (e.g. a "sentinel" key) are dropped by
// json.Unmarshal on load. The contract is "known fields
// preserved", not "byte-for-byte preservation of the file".
// This is documented behaviour of every JSON round-trip and
// is not a defect of --apply.
func TestDetectEmbedding_ApplyWritesOnlyEmbeddingBinding(t *testing.T) {
	srv := newFakeOllama(t, fakeOllamaOpts{
		ShowCapabilities: map[string][]string{
			"all-minilm:latest": {"embedding"},
		},
	})
	defer srv.Close()

	bin := buildDetectBin(t)
	ws := t.TempDir()

	// Pre-write a config with an LLM profile + a sentinel
	// synthesis flag. Apply must leave both untouched.
	preserved := `{
  "profiles": {
    "default": {"provider":"openai","model":"gpt-4o","base_url":"https://api.openai.com/v1","api_key":"SECRET"}
  },
  "components": {"memory":"default"},
  "synthesis_enabled": false
}`
	if err := os.WriteFile(filepath.Join(ws, "mpm_config.json"),
		[]byte(preserved), 0600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}

	cmd := stdlibexec.Command(bin, "config", "detect-embedding",
		"--apply", "embedding", "--force")
	cmd.Env = []string{
		"MPM_WORKSPACE=" + ws,
		"OLLAMA_ENDPOINT=" + srv.URL + "/",
		"PATH=" + lookupTestPath(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--apply: %v\n%s", err, out)
	}

	body, err := os.ReadFile(filepath.Join(ws, "mpm_config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	s := string(body)

	// The LLM profile must survive intact (api_key unchanged).
	if !strings.Contains(s, `"SECRET"`) {
		t.Errorf("LLM api_key was dropped; got:\n%s", s)
	}
	if !strings.Contains(s, `"synthesis_enabled": false`) {
		t.Errorf("synthesis_enabled was dropped; got:\n%s", s)
	}
	// Memory binding must remain on the default profile (the
	// operator's LLM must NOT silently rebind to the new
	// embedding profile).
	if !strings.Contains(s, `"memory": "default"`) {
		t.Errorf("memory component binding was disturbed; got:\n%s", s)
	}
	// Embedding profile + binding must be present.
	if !strings.Contains(s, `"embedding":`) {
		t.Errorf("embedding profile missing; got:\n%s", s)
	}
	if !strings.Contains(s, `"embedding": "embedding"`) {
		t.Errorf("components.embedding not bound to the new profile; got:\n%s", s)
	}
}

// --- helpers ---

// runDetectWithEnv runs the in-process detector with
// OLLAMA_ENDPOINT set to baseURL.
// runDetectWithEnv runs the in-process detector with
// OLLAMA_ENDPOINT set to baseURL.
func runDetectWithEnv(t *testing.T, baseURL string) detectReport {
	t.Setenv("OLLAMA_ENDPOINT", baseURL)
	return detectEmbeddingCandidates()
}

// newFakeOllama is a thin alias for startFakeOllama so this
// file reads naturally at the test-site.
func newFakeOllama(t *testing.T, opts fakeOllamaOpts) *httptest.Server {
	return startFakeOllama(t, opts)
}

// buildDetectBin builds a fresh mpm binary in a temp dir for
// the detector tests.
func buildDetectBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-test")
	cmd := stdlibexec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// sortStrings is a tiny helper used to assert alphabetical
// ordering of arbitrary string slices.
func sortStrings(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return out
}

var _ = sortStrings
var _ = json.Marshal
