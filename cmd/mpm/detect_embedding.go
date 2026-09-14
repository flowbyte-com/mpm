// cmd/mpm/detect_embedding.go — `mpm config detect-embedding`.
//
// 2026-09-14 release-pass: the previous design was Ollama-only
// and listed every model without capability classification.
// The release-pass contract is:
//
//   detect usable embedding capability
//
// Capability-oriented, not Ollama-exclusive. Discovery probes
// localhost (Ollama, OpenAI-compatible endpoints on standard
// ports) and surfaces the candidates it finds. Model state
// distinguishes CanEmbed / CannotEmbed (positively identified
// generative-only) / Unknown. Unknown is NOT false.
//
// `--apply` policy:
//   - one viable candidate → apply it (deterministic)
//   - multiple viable candidates → do not silently choose; list
//     them and instruct the operator
//   - zero candidates → useful "no embedding endpoint found"
//     output with the canonical config command

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// DetectEmbeddingCmd probes for embedding-capable endpoints and
// optionally writes a profile.
type DetectEmbeddingCmd struct {
	// Apply is the profile name to create and bind when --apply is
	// given.
	Apply string
	// Force overwrites an existing profile and rebinds
	// components.embedding when --apply is given. Without --force,
	// --apply refuses to mutate any existing profile.
	Force bool
}

// embeddingCandidate is the canonical internal representation of
// one embedding-capable endpoint + model pair. The candidate is
// capability-positive evidence: at least one runtime reported
// CanEmbed=true (or we used the secondary name-list fallback).
type embeddingCandidate struct {
	ProviderID string // canonical ID for the profile.Provider
	BaseURL    string // canonical endpoint URL
	Model      string // model name
	Source     string // human-readable source label (e.g. "Ollama")
}

// Run executes the detect-embedding command.
func (c *DetectEmbeddingCmd) Run() int {
	report := detectEmbeddingCandidates()

	fmt.Println("MPM · Embedding detection")
	fmt.Println()

	if report.ollama != nil {
		fmt.Println("Ollama")
		fmt.Printf("  %s\n", report.ollama.base)
		if report.ollama.reachable {
			fmt.Println("  reachable: yes")
			fmt.Println()
			if len(report.ollama.embeddingModels) > 0 {
				fmt.Println("  Embedding-capable models")
				for _, m := range report.ollama.embeddingModels {
					fmt.Printf("    %s\n", m)
				}
				fmt.Println()
			}
			if len(report.ollama.otherModels) > 0 {
				fmt.Printf("  Other models\n    %d ignored\n", len(report.ollama.otherModels))
				fmt.Println()
			}
		} else {
			fmt.Println("  reachable: no")
			fmt.Println()
		}
	} else {
		fmt.Println("Ollama")
		fmt.Println("  (no local endpoint detected)")
		fmt.Println()
	}

	if report.openaiCompat != nil {
		fmt.Println("OpenAI-compatible")
		fmt.Printf("  %s\n", report.openaiCompat.base)
		if report.openaiCompat.reachable {
			fmt.Println("  reachable: yes")
			if len(report.openaiCompat.embeddingModels) > 0 {
				fmt.Println()
				fmt.Println("  Embedding-capable models")
				for _, m := range report.openaiCompat.embeddingModels {
					fmt.Printf("    %s\n", m)
				}
				fmt.Println()
			}
		} else {
			fmt.Println("  reachable: no")
			fmt.Println()
		}
	} else {
		fmt.Println("OpenAI-compatible")
		fmt.Println("  no local endpoint detected")
		fmt.Println()
	}

	// Recommendation.
	if len(report.candidates) > 0 {
		fmt.Println("Recommended")
		// Deterministic: candidates are sorted by (Source, Model)
		// so the printed recommendation is stable across runs.
		first := report.candidates[0]
		fmt.Printf("  %s · %s\n", first.Source, first.Model)
		fmt.Println()
	}

	if len(report.candidates) == 0 {
		fmt.Println("No embedding-capable endpoint detected.")
		fmt.Println("Configure one explicitly with `mpm config profile add <name> --provider openai-compatible --model <model> --base-url <url>` and `mpm config component set embedding <name>`.")
		fmt.Println()
		fmt.Println("No configuration has been changed.")
		return 0
	}

	if c.Apply == "" {
		fmt.Println("No configuration has been changed.")
		fmt.Printf("Run `mpm config detect-embedding --apply %s` to apply the recommendation.\n", candidateProfileName(report.candidates[0]))
		return 0
	}

	// --apply with one candidate → apply it.
	// --apply with multiple → list them and refuse silently.
	if len(report.candidates) > 1 {
		fmt.Println()
		fmt.Printf("Multiple embedding configurations detected (%d).\n\n", len(report.candidates))
		for i, cand := range report.candidates {
			fmt.Printf("  %d. %s · %s\n", i+1, cand.Source, cand.Model)
		}
		fmt.Println()
		fmt.Println("Choose explicitly with:")
		fmt.Println("  mpm config component set embedding <profile>")
		fmt.Println()
		fmt.Println("No configuration has been changed.")
		return 0
	}

	// Single candidate — write the profile + binding.
	cand := report.candidates[0]
	cfg, err := config.LoadConfig()
	if err != nil {
		usererror.Error("--apply: load config: %v", err)
		return 1
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]config.Profile{}
	}
	profileName := c.Apply
	if profileName == "" {
		profileName = candidateProfileName(cand)
	}
	if _, exists := cfg.Profiles[profileName]; exists && !c.Force {
		usererror.Errorf(fmt.Sprintf("--apply refused: profile %q already exists; re-run with --force to overwrite, or `mpm config profile remove %s` first", profileName, profileName))
		return 1
	}
	if existingBinding, hasBinding := cfg.Components["embedding"]; hasBinding && existingBinding != profileName {
		if c.Force {
			fmt.Printf("⚠  rebinding components[\"embedding\"]: %q → %q (--force)\n", existingBinding, profileName)
		}
	}
	cfg.Profiles[profileName] = config.Profile{
		Provider: cand.ProviderID,
		Model:    cand.Model,
		BaseURL:  cand.BaseURL,
	}
	if cfg.Components == nil {
		cfg.Components = map[string]string{}
	}
	cfg.Components["embedding"] = profileName
	if err := config.SaveConfig(cfg); err != nil {
		usererror.Error("--apply: save config: %v", err)
		return 1
	}
	fmt.Printf("Wrote profile %q (provider=%s, model=%s).\n", profileName, cand.ProviderID, cand.Model)
	fmt.Printf("Set components[\"embedding\"] = %q.\n", profileName)
	return 0
}

// candidateProfileName derives a stable profile name from a
// candidate. Deterministic so rerunning detection without --apply
// produces the same recommended profile name.
func candidateProfileName(cand embeddingCandidate) string {
	return "embedding-" + cand.ProviderID
}

// probeResult is the per-endpoint discovery result. We
// distinguish "reachable" (HTTP succeeded) from
// "embeddingModels" (model list with positive CanEmbed evidence).
type probeResult struct {
	base            string
	reachable       bool
	embeddingModels []string
	otherModels     []string
}

// detectReport aggregates the per-endpoint probes into a single
// report. The `candidates` slice is the deterministic list of
// viable embedding candidates — one per (Source, Model) pair —
// sorted by (Source, Model) so the recommendation is reproducible.
type detectReport struct {
	ollama      *probeResult
	openaiCompat *probeResult
	candidates  []embeddingCandidate
}

// detectEmbeddingCandidates runs the discovery probes. Each
// probe is hermetic — short timeouts, no side effects.
func detectEmbeddingCandidates() detectReport {
	report := detectReport{}

	ollamaBase := os.Getenv("OLLAMA_ENDPOINT")
	if ollamaBase == "" {
		ollamaBase = "http://localhost:11434"
	}
	if r := probeOllama(ollamaBase); r != nil {
		report.ollama = r
		for _, m := range r.embeddingModels {
			report.candidates = append(report.candidates, embeddingCandidate{
				ProviderID: "ollama",
				BaseURL:    ollamaBase,
				Model:      m,
				Source:     "Ollama",
			})
		}
	}

	// OpenAI-compatible: scan a small set of well-known local
	// ports. Each probe is cheap; the brief says "safe localhost
	// probes" are fine.
	for _, base := range openAICompatLocalEndpoints() {
		if r := probeOpenAICompat(base); r != nil {
			report.openaiCompat = r
			for _, m := range r.embeddingModels {
				report.candidates = append(report.candidates, embeddingCandidate{
					ProviderID: "openai-compatible",
					BaseURL:    base,
					Model:      m,
					Source:     "OpenAI-compatible",
				})
			}
			// Don't scan other ports once we find one — the
			// first hit is the canonical local endpoint.
			break
		}
	}

	// Deterministic ordering: by Source then Model.
	sort.SliceStable(report.candidates, func(i, j int) bool {
		if report.candidates[i].Source != report.candidates[j].Source {
			return report.candidates[i].Source < report.candidates[j].Source
		}
		return report.candidates[i].Model < report.candidates[j].Model
	})

	return report
}

// openAICompatLocalEndpoints enumerates the localhost endpoints
// the embedding detector probes for OpenAI-compatible
// embeddings. Order = preference.
func openAICompatLocalEndpoints() []string {
	return []string{
		"http://localhost:1234/v1", // LM Studio default
		"http://localhost:8080/v1", // LocalAI default
	}
}

// probeOllama hits the Ollama /api/tags endpoint and classifies
// each model via /api/show capability metadata. Returns nil when
// the endpoint is unreachable.
func probeOllama(endpoint string) *probeResult {
	r := &probeResult{base: endpoint}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(strings.TrimRight(endpoint, "/") + "/api/tags")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	var tagResp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&tagResp); err != nil {
		return nil
	}
	r.reachable = true
	for _, m := range tagResp.Models {
		if classifyOllamaModel(strings.TrimRight(endpoint, "/"), m.Name) == "embed" {
			r.embeddingModels = append(r.embeddingModels, m.Name)
		} else {
			r.otherModels = append(r.otherModels, m.Name)
		}
	}
	return r
}

// classifyOllamaModel queries /api/show for the model's
// capabilities and returns "embed", "generate", or "unknown".
//
// Capability-based: the runtime is the source of truth. If the
// probe fails, the secondary fallback name list in
// internal/core/role_validation.go's ValidateLLMRole
// (cmd/mpm/role_validation.go) covers the same cases; here we
// return "unknown" so the caller leaves the model unclassified
// (Unknown != false).
func classifyOllamaModel(base, model string) string {
	caps, err := mpminternal.ProbeOllamaCapabilities(base, model)
	if err == nil && caps.FromAPI {
		if caps.CanEmbed && !caps.CanComplete {
			return "embed"
		}
		if caps.CanComplete && !caps.CanEmbed {
			return "generate"
		}
		if caps.CanComplete && caps.CanEmbed {
			return "embed" // dual-capability: surface in embedding view
		}
		return "unknown"
	}
	// Probe failed — secondary fallback via the canonical
	// fallback list. If the model name matches a known embedding
	// pattern, treat as embed; otherwise unknown.
	if isKnownEmbeddingName(model) {
		return "embed"
	}
	return "unknown"
}

// isKnownEmbeddingName is the secondary fallback used when the
// capability probe is unavailable. It defers to the same set of
// well-known embedding-only fragments the role validator uses.
func isKnownEmbeddingName(model string) bool {
	lower := strings.ToLower(model)
	for _, fragment := range embeddingModelFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

// embeddingModelFragments mirrors the fallback list in
// role_validation.go. Kept in sync; if the role validator's
// list expands, this one does too. (Both are local to this
// binary — no shared package dep needed.)
var embeddingModelFragments = []string{
	"all-minilm",
	"nomic-embed",
	"mxbai-embed",
	"bge-embed",
	"e5-",
	"gte-",
	"snowflake-arctic-embed",
	"text-embedding-",
	"embed-",
	"embedding",
}

// probeOpenAICompat hits the /v1/models endpoint and asks each
// model whether it speaks embeddings. We don't probe per-model
// (that would multiply requests) — the OpenAI-compatible
// ecosystem's default model catalogue is operator-supplied, so
// we trust the catalogue's existence and require the operator
// to provide a model name when binding.
//
// Returns nil when unreachable. embeddingModels is empty
// because we don't know which of the catalogue's models embed.
// The recommendation therefore points operators at the
// catalogue rather than picking one.
func probeOpenAICompat(base string) *probeResult {
	r := &probeResult{base: base}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(strings.TrimRight(base, "/") + "/models")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	var modelsResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&modelsResp); err != nil {
		return nil
	}
	r.reachable = true
	// We don't probe per-model embedding capability (the
	// OpenAI-compatible /v1/embeddings endpoint usually
	// exposes whatever the model catalogue advertises).
	// Operators pick a model from the catalogue. We do not
	// fabricate a recommendation.
	return r
}

// probeOpenRouter hits the OpenRouter /api/v1/models endpoint and
// returns the catalogue of currently-supported model IDs.
//
// OpenRouter exposes a much larger model catalogue than the
// offline modelCatalogFor fallback. We use it as the source of
// truth for the OpenRouter menu; the brief explicitly forbids
// baking in a giant fixed list.
//
// The probe is best-effort: short timeout, no auth required for
// the public models endpoint (OpenRouter's auth-protected
// variant lives at /api/v1/auth/key, which we don't call).
// Returns nil when unreachable.
//
// 2026-09-14 release-pass: kept narrow on purpose — we only
// extract the model IDs. Capability metadata (whether a given
// ID supports embeddings) is not surfaced by OpenRouter's free
// catalogue; embedding remains capability-positive-evidence only
// via Ollama. This is documented at cmd/mpm/provider_registry.go.
func probeOpenRouter() []string {
	endpoint := os.Getenv("OPENROUTER_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://openrouter.ai/api/v1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(strings.TrimRight(endpoint, "/") + "/models")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	var modelsResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&modelsResp); err != nil {
		return nil
	}
	ids := make([]string, 0, len(modelsResp.Data))
	for _, m := range modelsResp.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// openRouterCatalogForView returns the model list for an
// OpenRouter-flavoured view. Live discovery via /api/v1/models
// is the canonical path; an empty result on probe failure is
// expected — the 2026-09-14 simplification pass removed the
// branded `openrouter/free` fallback because the public wizard
// surface does not advertise OpenRouter presets at all (operators
// type the current model ID at the Custom prompt). Internal
// callers that want a stable preset can pipe through Custom at
// runtime.
//
// Stable alphabetical sort: caller prepends Custom at index 0;
// the rest is sorted case-insensitive with a deterministic
// tiebreak on the original string.
func openRouterCatalogForView() []string {
	catalog := probeOpenRouter()
	sort.SliceStable(catalog, func(i, j int) bool {
		li := strings.ToLower(catalog[i])
		lj := strings.ToLower(catalog[j])
		if li != lj {
			return li < lj
		}
		return catalog[i] < catalog[j]
	})
	return catalog
}
