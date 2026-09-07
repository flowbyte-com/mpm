// Package synth provides the LLM HTTP client used for memory synthesis and
// admission. It is the active counterpart to internal/config (passive state):
// config holds what vendors are declared, synth holds how to talk to them.
//
// Import direction: synth imports config (to read vendor configuration).
// config does NOT import synth — that boundary is the single-responsibility
// split between "what" (config) and "how" (synth).
package synth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

// NewSynthClient reads LLM configuration from mpm_config.json, returning a
// ready-to-use SynthClient. Defaults to MiniMax.
//
// Resolution order (canonical first, legacy last):
//
//  1. Profiles["default"] (canonical surface — what the wizard writes)
//  2. Components["synth"] binding → Profiles[<binding>] (canonical routing)
//  3. Legacy Synth block (one-way migration path for pre-profiles configs)
//  4. Hardcoded defaults + env-var API key (last-resort)
//
// Precedence: when both a profile and a legacy Synth block are present,
// the profile wins. The legacy Synth block is a compatibility read for
// installs that haven't migrated yet; modern configs configure
// Profiles["default"] (or bind Components["synth"] to a named profile).
func NewSynthClient() *SynthClient {
	cfg, err := config.LoadConfig()
	sc := &SynthClient{
		Model:     "MiniMax-M2.7",
		BaseURL:   "https://api.minimax.io/anthropic/v1",
		MaxTokens: 1024,
		Timeout:   300 * time.Second,
	}
	// 1. Legacy Synth block first — sets the baseline. Modern profiles
	// below override the legacy fields.
	if err == nil && cfg != nil && cfg.Synth != nil {
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
	// 2. Canonical: Profiles["default"] (resolved through ProfileFor so an
	// explicit Components["synth"] binding also wins). Overrides the
	// legacy Synth block above.
	if err == nil && cfg != nil {
		if prof := cfg.ProfileFor("synth"); prof != nil {
			if prof.Model != "" {
				sc.Model = prof.Model
			}
			if prof.BaseURL != "" {
				sc.BaseURL = prof.BaseURL
			}
			if prof.MaxTokens > 0 {
				sc.MaxTokens = prof.MaxTokens
			}
			if prof.TimeoutSecs > 0 {
				sc.Timeout = time.Duration(prof.TimeoutSecs) * time.Second
			}
			if prof.APIKey != "" {
				sc.APIKey = prof.APIKey
			}
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
	// Loud, structured failure signal. Every source of credentials the
	// synth client knows about — Profiles["default"] (canonical),
	// cfg.Synth (legacy migration), and the env-var fallbacks — has
	// been exhausted without finding an API key. Without this log, the
	// failure surfaces only at request time when the LLM call returns
	// a 401. Loud failure beats silent spinning.
	if sc.APIKey == "" {
		slog.Error("synth: no API key configured",
			"component", "synth",
			"surface_exhausted", []string{
				"cfg.Profiles[default].api_key (canonical)",
				"cfg.Profiles[<Components[synth] binding>].api_key (canonical)",
				"cfg.Synth.api_key (legacy migration)",
				"env MINIMAX_API_KEY (default wire)",
				"env OPENAI_API_KEY (openai wire)",
				"env OPENROUTER_API_KEY (any wire)",
			},
			"remediation", "set api_key on cfg.Profiles[default] (canonical) or cfg.Synth.api_key (legacy migration) or one of the env vars above; do not commit the secret to version control — inject at runtime",
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
//
// Transport routed through doLLMRequest (post-M3 audit H-1): the
// prior inline HTTP code here was the gold-standard for wire-aware
// dispatch; the audit found that compact.go and admission.go had
// diverged from it. Pulling all three sites through the helper is
// the structural fix.
func (sc *SynthClient) Synthesize(ctx context.Context, fragments []string) (*SynthResult, error) {
	if sc.APIKey == "" {
		return nil, fmt.Errorf("no API key configured (set api_key in mpm_config.json synth block or appropriate env var for the configured wire)")
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

	respBody, err := sc.DoLLMRequest(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("synthesis [vendor=%s]: %w", wireLabel(sc.Wire), err)
	}

	rawResult, err := sc.ParseResponseBody(respBody, "synthesis")
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
