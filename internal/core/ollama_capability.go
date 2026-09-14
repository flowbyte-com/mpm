// internal/core/ollama_capability.go — Capability probe for
// Ollama-served models. Used by the LLM-role validation
// boundary (cmd/mpm/role_validation.go) to reject embedding-only
// models when the operator tries to configure them as LLM
// providers via `mpm config` or `mpm config profile set`.
//
// 2026-09-14 release-pass: MPM's LLM wizard historically
// accepted any model name, including embedding-only models
// like `all-minilm`. The wizard would then ask generation-
// specific fields (Max tokens) and silently produce a broken
// LLM config. The fix: capability-based detection.
//
// Ollama exposes model capabilities through two endpoints:
//
//   POST /api/show  {"name": "<model>"} → {"capabilities": ["completion", ...]}
//   POST /api/tags                      → {"models": [{"details": {"capabilities": [...]}}]}
//
// Newer Ollama versions (>=0.3.x) populate `details.capabilities`.
// Older versions or non-Ollama endpoints may not. We try /api/show
// first (more authoritative per-model), then fall back to
// /api/tags. If neither yields capability metadata we return
// `ModelCapabilities{}` (both flags false = "unknown") so the
// caller can decide whether to accept (custom provider / unknown
// capability = preserve flexibility per the brief).

package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ModelCapabilities reports the authoritative role classification
// for a model served by an Ollama-compatible endpoint. Either
// flag is true when the endpoint's metadata positively reports
// the capability. Both false means "unknown" — the caller
// should NOT reject on unknown (the brief explicitly allows
// custom providers / unknown capability).
type ModelCapabilities struct {
	CanComplete bool `json:"can_complete"`
	CanEmbed    bool `json:"can_embed"`
	// FromAPI is true when at least one Ollama endpoint
	// returned capability metadata. When false, both flags
	// are zero and callers MUST NOT use them as evidence.
	FromAPI bool `json:"from_api"`
}

// String renders a stable diagnostic label. Used by tests and
// the role-validation boundary's failure message.
func (c ModelCapabilities) String() string {
	if !c.FromAPI {
		return "unknown"
	}
	parts := []string{}
	if c.CanComplete {
		parts = append(parts, "completion")
	}
	if c.CanEmbed {
		parts = append(parts, "embedding")
	}
	if len(parts) == 0 {
		return "no-capabilities"
	}
	return strings.Join(parts, "+")
}

// IsEmbeddingOnly reports whether the model is positively known
// to support embedding but NOT completion. This is the
// reject-on-evidence condition the brief calls out: we reject
// only when the runtime positively says "embedding-only", never
// on uncertainty.
func (c ModelCapabilities) IsEmbeddingOnly() bool {
	return c.FromAPI && c.CanEmbed && !c.CanComplete
}

// ProbeOllamaCapabilities queries the Ollama-compatible endpoint
// at baseURL for the capabilities of model. baseURL may be any
// of: a host (`http://localhost:11434`), an /api/embed path,
// or an /api/embeddings legacy path — the helper normalises
// to /api/show and /api/tags.
//
// Returns ModelCapabilities{FromAPI: false} when no capability
// metadata can be obtained (timeout, non-Ollama endpoint, model
// not installed, etc.). The caller is responsible for
// distinguishing "unknown" from "embedding-only" — see
// IsEmbeddingOnly.
func ProbeOllamaCapabilities(baseURL, model string) (ModelCapabilities, error) {
	if baseURL == "" {
		return ModelCapabilities{}, fmt.Errorf("probe: empty base URL")
	}
	if model == "" {
		return ModelCapabilities{}, fmt.Errorf("probe: empty model")
	}
	host, err := normalizeOllamaHost(baseURL)
	if err != nil {
		return ModelCapabilities{}, fmt.Errorf("probe: invalid base URL: %w", err)
	}

	// Try /api/show first — most authoritative per-model.
	if caps, ok := probeOllamaShow(host, model); ok {
		return caps, nil
	}
	// Fallback to /api/tags — newer Ollama exposes capabilities
	// on the tags listing's details block.
	if caps, ok := probeOllamaTags(host, model); ok {
		return caps, nil
	}
	// Neither endpoint returned usable metadata.
	return ModelCapabilities{}, nil
}

// normalizeOllamaHost accepts the same URL shapes
// OllamaProvider does (bare host, /api/embed, /api/embeddings)
// and returns the host with no path AND no trailing slash so
// the helper can build /api/show and /api/tags paths without
// producing double slashes. Trailing slash matters because
// `host + "/api/show"` becomes `host//api/show` otherwise, and
// some Ollama-compatible endpoints (LocalAI, test fakes)
// route by exact path.
func normalizeOllamaHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid ollama URL: %q", raw)
	}
	// Strip any /api/* suffix.
	if u.Path != "" && u.Path != "/" {
		u.Path = ""
	}
	// url.Parse keeps a trailing "/" if the input had one. Strip
	// the path explicitly so the returned host has no trailing
	// slash — otherwise `host + "/api/show"` doubles up.
	result := u.String()
	result = strings.TrimSuffix(result, "/")
	return result, nil
}

// probeOllamaShow POSTs /api/show and parses the
// `capabilities` array.
func probeOllamaShow(host, model string) (ModelCapabilities, bool) {
	body, _ := json.Marshal(map[string]string{"name": model})
	resp, err := shortTimeoutPost(host+"/api/show", body)
	if err != nil {
		return ModelCapabilities{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ModelCapabilities{}, false
	}
	var out struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ModelCapabilities{}, false
	}
	caps := classifyCapabilities(out.Capabilities)
	if caps.FromAPI {
		return caps, true
	}
	// Some Ollama versions don't return capabilities at /api/show
	// but include the model info under a different shape.
	// Treat as "no metadata" rather than fabricating evidence.
	return ModelCapabilities{}, false
}

// probeOllamaTags POSTs /api/tags and parses the per-model
// `details.capabilities` array.
func probeOllamaTags(host, model string) (ModelCapabilities, bool) {
	resp, err := shortTimeoutPost(host+"/api/tags", nil)
	if err != nil {
		return ModelCapabilities{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ModelCapabilities{}, false
	}
	var out struct {
		Models []struct {
			Name    string `json:"name"`
			Details struct {
				Capabilities []string `json:"capabilities"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ModelCapabilities{}, false
	}
	for _, m := range out.Models {
		if !strings.EqualFold(m.Name, model) && !strings.HasPrefix(strings.ToLower(m.Name), strings.ToLower(model)+":") {
			continue
		}
		caps := classifyCapabilities(m.Details.Capabilities)
		if caps.FromAPI {
			return caps, true
		}
	}
	return ModelCapabilities{}, false
}

// classifyCapabilities maps the Ollama capability strings to
// the canonical ModelCapabilities. Recognised values:
//
//   "completion" or "generate" or "chat"  → CanComplete = true
//   "embedding"                            → CanEmbed = true
//
// Unknown capability strings are ignored (do not fabricate
// evidence). When both flags are still false after classification
// the returned struct has FromAPI=false so callers can
// distinguish "no metadata" from "metadata says no capabilities".
func classifyCapabilities(raw []string) ModelCapabilities {
	caps := ModelCapabilities{}
	for _, s := range raw {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "completion", "generate", "chat":
			caps.CanComplete = true
		case "embedding":
			caps.CanEmbed = true
		}
	}
	caps.FromAPI = caps.CanComplete || caps.CanEmbed
	return caps
}

// shortTimeoutPost is a tiny helper that issues a POST with a
// short timeout so the LLM wizard doesn't block on a slow /
// unreachable Ollama endpoint during the operator's interactive
// session. 2 seconds is enough for localhost Ollama and short
// enough to be invisible on a flaky remote endpoint.
func shortTimeoutPost(url string, body []byte) (*http.Response, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}
