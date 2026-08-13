// Package synth provides the LLM HTTP client used for memory synthesis and
// admission. It is the active counterpart to internal/config (passive state):
// config holds what vendors are declared, synth holds how to talk to them.
//
// Import direction: synth imports config (to read vendor configuration).
// config does NOT import synth — that boundary is the single-responsibility
// split between "what" (config) and "how" (synth).
package synth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// SynthClientInterface is the contract the synthesis worker consumes.
// Tests inject a mock via NewSynthesisWorkerForTest; production code
// receives *SynthClient from NewSynthClient.
//
// Note: ParseResponseBody is NOT part of this interface because it is only
// used by admission.EvaluateCandidate, which takes the concrete *SynthClient
// (not the interface) — keeping ParseResponseBody out of the interface
// means test mocks for the synthesis worker don't have to stub it.
type SynthClientInterface interface {
	Synthesize(ctx context.Context, fragments []string) (*SynthResult, error)
	SynthesizeWithVendor(ctx context.Context, vendor config.SynthVendor, content string, tags []string) (*SynthResult, error)
}

// SynthClient is the HTTP client to LLM vendors. Fields are exported so
// callers (admission flow, CLI handlers) can read the resolved configuration
// without going through a getter.
//
// Wire selection (Anthropic-protocol vs OpenAI-protocol) is inferred from
// BaseURL at construction. The Model/BaseURL/APIKey set here shape the
// wire — operators don't pick the wire explicitly; URL is the operative
// signal. See internal/core/synth/wire.go for the dispatch table.
type SynthClient struct {
	Model     string
	APIKey    string
	BaseURL   string
	MaxTokens int
	Timeout   time.Duration
	// Wire is the protocol the client will speak. Resolved once at
	// construction time from BaseURL; defaults to Anthropic-protocol
	// for backwards compatibility. Exported for tests and for the
	// orchestrator's ModelFactory which sometimes needs to render the
	// wire label to operators.
	Wire wireShape
}

// NewSynthClient reads LLM configuration from mpm_config.json (synth block)
// and env vars, returning a ready-to-use SynthClient. Defaults to MiniMax.
// Missing values fall back to defaults or env vars.
func NewSynthClient() *SynthClient {
	cfg, err := config.LoadConfig()
	sc := &SynthClient{
		Model:     "MiniMax-M2.7",
		BaseURL:   "https://api.minimax.io/anthropic/v1",
		MaxTokens: 1024,
		Timeout:   300 * time.Second,
	}
	if err == nil && cfg.Synth != nil {
		if cfg.Synth.Model != "" {
			sc.Model = cfg.Synth.Model
		}
		if cfg.Synth.BaseURL != "" {
			sc.BaseURL = cfg.Synth.BaseURL
		}
		if cfg.Synth.MaxTokens > 0 {
			sc.MaxTokens = cfg.Synth.MaxTokens
		}
		if cfg.Synth.TimeoutSecs > 0 {
			sc.Timeout = time.Duration(cfg.Synth.TimeoutSecs) * time.Second
		}
		if cfg.Synth.APIKey != "" {
			sc.APIKey = cfg.Synth.APIKey
		}
	}
	// Profile fallback (added 2026-08-13). The legacy synth block was
	// the original single-source for LLM credentials, but the
	// canonical path operators are encouraged to use is the
	// Profiles map (Profiles["default"] or any binding via the
	// Components map). If cfg.Synth.APIKey is empty — a common
	// drift because the two locations are easy to forget to keep
	// in sync — fall back to the resolved profile's APIKey. This
	// aligns the synth client with the rest of the substrate
	// (admission, planner, reviewer, etc) which already route
	// through cfg.ProfileFor(component). Without this fallback,
	// every install that configured profiles correctly but left
	// the legacy synth block empty would hit
	// "no API key configured" on compact_epistemology and the
	// rest of the synthesis surface — a silent-failure class
	// that was masked by an invisible env-var fallback when a
	// developer happened to have MINIMAX_API_KEY in their shell.
	if sc.APIKey == "" {
		if prof := cfg.ProfileFor("synth"); prof != nil && prof.APIKey != "" {
			sc.APIKey = prof.APIKey
		}
	}
	if sc.APIKey == "" {
		// Env-var fallback. The legacy MINIMAX_API_KEY works
		// for the default Anthropic-protocol wire (which is
		// what MiniMax itself speaks). For OpenAI-protocol
		// wires (OpenRouter, OpenAI native, LM Studio),
		// OPENROUTER_API_KEY or OPENAI_API_KEY are the
		// matching env vars. Wire inference picks the right
		// one based on BaseURL.
		sc.Wire = inferWire(sc.BaseURL)
		switch sc.Wire {
		case wireOpenAI:
			if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
				sc.APIKey = key
			} else if key := os.Getenv("OPENAI_API_KEY"); key != "" {
				sc.APIKey = key
			}
		default:
			if key := os.Getenv("MINIMAX_API_KEY"); key != "" {
				sc.APIKey = key
			} else if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
				sc.APIKey = key
			}
		}
	} else {
		// API key was set on the explicit config path
		// (synth block or profile); wire is still inferred
		// from BaseURL the same way.
		sc.Wire = inferWire(sc.BaseURL)
	}
	// Loud, structured failure signal (added 2026-08-13). Every
	// source of credentials the synth client knows about — the
	// legacy cfg.Synth block, the resolved cfg.ProfileFor(synth)
	// profile, and the env-var fallbacks MINIMAX_API_KEY /
	// OPENAI_API_KEY / OPENROUTER_API_KEY — has been exhausted
	// without finding an API key. Without this log, the failure
	// surfaces only at request time when the LLM call returns a
	// 401, well after the substrate has been alive long enough to
	// appear healthy. Compaction and admission both spin on
	// "no API key configured" errors silently otherwise. Loud
	// failure beats silent spinning.
	if sc.APIKey == "" {
		slog.Error("synth: no API key configured",
			"component", "synth",
			"surface_exhausted", []string{
				"cfg.Synth.api_key",
				"cfg.ProfileFor(synth).api_key",
				"env MINIMAX_API_KEY (default wire)",
				"env OPENAI_API_KEY (openai wire)",
				"env OPENROUTER_API_KEY (any wire)",
			},
			"remediation", "set api_key on cfg.Profiles[default] (canonical) or cfg.Synth.api_key (legacy) or one of the env vars above; do not commit the secret to version control — inject at runtime",
		)
	}
	return sc
}

// synthesisSystemPrompt is the self-contained prompt for memory consolidation.
// Kept private because external callers should not need it — admission.go has
// its own admissionSystemPrompt.
const synthesisSystemPrompt = `You are a senior archivist tasked with consolidating overlapping notes into a single, coherent record. Preserve all factual claims faithfully — do not add, infer, or hallucinate any facts not present in the input.

INPUT:
You will receive N memory fragments separated by "---MEMORY---". Each fragment may share overlapping content with the others. Your job is to produce a SINGLE consolidated memory.

RULES:
1. PRESERVE all factual claims from every fragment. Do not drop information.
2. REMOVE only pure redundancy — if two fragments say the same thing in different words, rephrase once in the output. Do not decide one version is "better" and discard it.
3. NEVER add new facts, conclusions, or interpretations not present in the input.
4. PRESERVE all tags from the input. Merge duplicates. Do not invent new tags.
5. OUTPUT a JSON object with two fields:
   - "content": the synthesized memory text (max ~500 words)
   - "tags": the merged and deduplicated tag array
6. The output must be valid JSON. No markdown, no explanation, no preamble.

EXAMPLE:
Input fragment 1: "v prefers short messages. Prefers direct communication."
Input fragment 2: "User v — short and direct. Doesn't like fluff."
Output: {"content": "v prefers short, direct communication. No fluff.", "tags": ["preference", "v", "communication"]}`

// SynthResult is the LLM output structure for synthesis calls.
// Exported because SynthClientInterface returns it.
type SynthResult struct {
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
}

// Synthesize sends memory fragments to the LLM using the consolidation prompt
// and returns the parsed synthesis result. Uses a context with the configured
// timeout for cancellation safety.
//
// Wire dispatch: Auth header, request path, and response parser are
// selected by sc.Wire (inferred from BaseURL at construction). Same
// body shape on both wires (system message first, user second in the
// messages array) — the divergence is in the URL, the header, and the
// response unwrap, all of which live in wire.go.
func (sc *SynthClient) Synthesize(ctx context.Context, fragments []string) (*SynthResult, error) {
	if sc.APIKey == "" {
		return nil, fmt.Errorf("no API key configured (set api_key in mpm_config.json synth block, MINIMAX_API_KEY env var, or OPENROUTER_API_KEY env var)")
	}

	userContent := strings.Join(fragments, "\n---MEMORY---\n")

	body := map[string]interface{}{
		"model":      sc.Model,
		"max_tokens": sc.MaxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": synthesisSystemPrompt},
			{"role": "user", "content": userContent},
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", sc.BaseURL+sc.Wire.path(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	authName, authValue := sc.Wire.authHeader(sc.APIKey)
	req.Header.Set(authName, authValue)

	var resp *http.Response
	var respBody []byte

	for attempt := 0; attempt <= 1; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}

		client := &http.Client{Timeout: sc.Timeout}
		resp, err = client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("API request failed: %w", err)
		}

		respBody, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		if resp.StatusCode == 200 {
			break // success
		}

		// Retry once on server errors (5xx), bail on everything else
		if attempt == 0 && resp.StatusCode >= 500 && resp.StatusCode < 600 {
			continue
		}
		return nil, fmt.Errorf("API returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	rawResult, err := sc.Wire.parseResponseBody(respBody)
	if err != nil {
		return nil, fmt.Errorf("synthesis [vendor=%s]: %w", wireLabel(sc.Wire), err)
	}
	var result SynthResult
	if err := json.Unmarshal(rawResult, &result); err != nil {
		return nil, fmt.Errorf("failed to parse synthesis JSON: %w (raw: %s)", err, string(rawResult))
	}
	if result.Content == "" {
		return nil, fmt.Errorf("LLM returned empty synthesized content")
	}
	return &result, nil
}

// wireLabel returns a human-readable name for a wire. Used in error
// messages to make watchdog output diagnosable without inspecting
// raw JSON.
func wireLabel(w wireShape) string {
	switch w {
	case wireOpenAI:
		return "openai"
	default:
		return "default"
	}
}

// ParseResponseBody parses a vendor response body and returns the
// inner LLM output text as raw bytes. Exported because admission.go
// and compact_epistemology (different packages) need to call it on
// their own vendor responses.
//
// The vendor parameter is included for contextual error messages —
// the caller passes the vendor name (or "default" / "admission" /
// "compact_lesson" / etc.) so error logs identify which path
// produced the failure.
//
// Wire dispatch is delegated to sc.Wire (set by NewSynthClient
// from BaseURL). The exported method is the *same* dispatch as
// Synthesize uses internally; admission and compact inherit the
// wire for free without choosing.
func (sc *SynthClient) ParseResponseBody(body []byte, vendor string) ([]byte, error) {
	raw, err := sc.Wire.parseResponseBody(body)
	if err != nil {
		return nil, fmt.Errorf("vendor %s: %w", vendor, err)
	}
	return raw, nil
}
