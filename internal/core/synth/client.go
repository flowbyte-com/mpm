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
//
// Output-token limits: NOT user-configurable. The substrate supplies
// its own internal max_tokens value at wire time; any
// Profiles[...].MaxTokens or legacy cfg.Synth.MaxTokens values are
// IGNORED at construction time. The field stays on the profile struct
// for backwards-compatible JSON parsing (legacy alpha installs may
// still have it on disk), but the runtime never honors it. This
// prevents low values such as max_tokens=10 from silently truncating
// long-running synthesis jobs through user configuration.
func NewSynthClient() *SynthClient {
	cfg, err := config.LoadConfig()
	sc := &SynthClient{
		Model:     "MiniMax-M2.7",
		BaseURL:   "https://api.minimax.io/anthropic/v1",
		MaxTokens: synthInternalMaxTokens, // substrate-supplied default; never user-configurable
		Timeout:   300 * time.Second,
	}
	// 1. Legacy Synth block first — sets the baseline. Modern profiles
	// below override the legacy fields. MaxTokens is intentionally
	// NOT read here — see the function-level comment above.
	if err == nil && cfg != nil && cfg.Synth != nil {
		if cfg.Synth.Model != "" {
			sc.Model = cfg.Synth.Model
		}
		if cfg.Synth.BaseURL != "" {
			sc.BaseURL = cfg.Synth.BaseURL
		}
		// MaxTokens: ignored. Substrate default applies.
		if cfg.Synth.TimeoutSecs > 0 {
			sc.Timeout = time.Duration(cfg.Synth.TimeoutSecs) * time.Second
		}
		if cfg.Synth.APIKey != "" {
			sc.APIKey = cfg.Synth.APIKey
		}
	}
	// 2. Canonical: Profiles["default"] (resolved through ProfileFor so an
	// explicit Components["synth"] binding also wins). Overrides the
	// legacy Synth block above. MaxTokens is intentionally NOT read.
	if err == nil && cfg != nil {
		if prof := cfg.ProfileFor("synth"); prof != nil {
			if prof.Model != "" {
				sc.Model = prof.Model
			}
			if prof.BaseURL != "" {
				sc.BaseURL = prof.BaseURL
			}
			// MaxTokens: ignored. Substrate default applies.
			if prof.TimeoutSecs > 0 {
				sc.Timeout = time.Duration(prof.TimeoutSecs) * time.Second
			}
			if prof.APIKey != "" {
				sc.APIKey = prof.APIKey
			}
		}
	}
	if sc.APIKey == "" {
		// Env-var fallback. Each provider reads its OWN
		// env var only — no cross-provider fallthrough.
		//
		// 2026-09-14 release-pass credential-isolation:
		// OpenRouter must NOT consume OPENAI_API_KEY and
		// vice versa. Generic OpenAI-compatible endpoints
		// require the dedicated OAI_COMPAT_API_KEY env
		// var (or an explicit profile api_key). The
		// provider-specific lookup is keyed off the
		// canonical base URL's domain substring.
		//
		// This is a strict lookup: no provider can
		// accidentally inherit another provider's secret.
		sc.Wire = inferWire(sc.BaseURL)
		lu := strings.ToLower(sc.BaseURL)
		switch sc.Wire {
		case wireOpenAI:
			switch {
			case strings.Contains(lu, "openrouter"):
				if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
					sc.APIKey = key
				}
			case strings.Contains(lu, "googleapis"):
				if key := os.Getenv("GEMINI_API_KEY"); key != "" {
					sc.APIKey = key
				} else if key := os.Getenv("GOOGLE_API_KEY"); key != "" {
					sc.APIKey = key
				}
			case strings.Contains(lu, "x.ai"):
				if key := os.Getenv("XAI_API_KEY"); key != "" {
					sc.APIKey = key
				}
			case strings.Contains(lu, "mistral.ai"):
				if key := os.Getenv("MISTRAL_API_KEY"); key != "" {
					sc.APIKey = key
				}
			case strings.Contains(lu, "cohere.com"):
				if key := os.Getenv("COHERE_API_KEY"); key != "" {
					sc.APIKey = key
				}
			case strings.Contains(lu, "openai.com"),
				strings.Contains(lu, "/openai/"),
				strings.HasSuffix(lu, "/openai"):
				if key := os.Getenv("OPENAI_API_KEY"); key != "" {
					sc.APIKey = key
				}
			default:
				// Generic OpenAI-compatible endpoints:
				// require the dedicated OAI_COMPAT_API_KEY env
				// var. OPENAI_API_KEY and OPENROUTER_API_KEY
				// are NEVER read here — operators with a local
				// LM Studio / LocalAI / vLLM endpoint MUST
				// configure a separate key (or set the
				// profile's api_key explicitly).
				if key := os.Getenv("OAI_COMPAT_API_KEY"); key != "" {
					sc.APIKey = key
				}
			}
		default:
			if key := os.Getenv("MINIMAX_API_KEY"); key != "" {
				sc.APIKey = key
			} else if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
				sc.APIKey = key
			}
		}
	} else {
		// API key was set on the explicit config path
		// (synth block or profile); wire is still inferred
		// from BaseURL the same way.
		sc.Wire = inferWire(sc.BaseURL)
	}
	// 2026-09-10 cleanup: synthesis is OPTIONAL — a successful memory
	// save must not be reported as ERROR-level failure just because the
	// operator hasn't configured an LLM provider. Distinguish:
	//   • provider not configured → warn (informational; safe default)
	//   • provider configured but request failed → error (real failure)
	// Pre-fix this logged at ERROR, which made `mpm remember` / `mpm
	// memory add` look broken even when the save itself succeeded.
	// The same structural info is preserved (every surface exhausted,
	// remediation hint) but at a severity that matches the situation.
	if sc.APIKey == "" {
		slog.Warn("synth: no API key configured — synthesis is optional and will be skipped; memory save itself is unaffected",
			"component", "synth",
			"surface_exhausted", []string{
				"cfg.Profiles[default].api_key (canonical)",
				"cfg.Profiles[<Components[synth] binding>].api_key (canonical)",
				"cfg.Synth.api_key (legacy migration)",
				"env ANTHROPIC_API_KEY (anthropic wire)",
				"env MINIMAX_API_KEY (anthropic wire)",
				"env OPENAI_API_KEY (openai wire, openai.com)",
				"env OPENROUTER_API_KEY (openai wire, openrouter)",
				"env GEMINI_API_KEY (openai wire, googleapis)",
				"env XAI_API_KEY (openai wire, x.ai)",
				"env MISTRAL_API_KEY (openai wire, mistral.ai)",
				"env COHERE_API_KEY (openai wire, cohere.com)",
				"env OAI_COMPAT_API_KEY (openai wire, generic compatible)",
			},
			"remediation", "set api_key on cfg.Profiles[default] (canonical) or cfg.Synth.api_key (legacy migration) or the dedicated *_API_KEY env var for the chosen provider; do not commit the secret to version control — inject at runtime",
		)
	}
	return sc
}

// synthesisSystemPrompt is the self-contained prompt for memory consolidation.
// Kept private because external callers should not need it — admission.go has
// its own admissionSystemPrompt.
//
// 2026-09-14 release-pass: the prompt explicitly permits
// uncertainty as a TERMINAL result. The model is instructed
// NOT to invent missing facts merely to make memories
// synthesize cleanly. A single coherent conclusion is one
// acceptable outcome; conflicting evidence, ambiguity, undefined
// proposition, or insufficient evidence are equally acceptable.
// The synthesis safeguard contract relies on this — retry loops
// are bounded by the safeguard, not by the model re-reasoning
// on every "uncertain" response.
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

ACCEPTABLE TERMINAL OUTCOMES (any one is a successful synthesis):
- a coherent consolidated memory
- a note that the source is ambiguous, contradictory,
  mathematically undefined, or insufficient — preserve the
  observable quality rather than force coherence
- an explicit statement that no reliable conclusion can be
  derived from the input

You must NEVER invent missing facts, fabricate values, or
re-interpret contradictory inputs as if one were correct
just so the synthesis can be produced. Uncertainty is a valid
result, not a reason to keep calling.

EXAMPLE:
Input fragment 1: "v prefers short messages. Prefers direct communication."
Input fragment 2: "User v — short and direct. Doesn't like fluff."
Output: {"content": "v prefers short, direct communication. No fluff.", "tags": ["preference", "v", "communication"]}

EXAMPLE (uncertainty is preserved):
Input fragment 1: "square root of divided by zero x 0"
Output: {"content": "The source statement is mathematically undefined; no reliable conclusion can be derived.", "tags": []}`

// synthInternalMaxTokens is the substrate-supplied wire cap used
// when calling LLM APIs. NOT user-configurable — operators must
// not be able to set this through `mpm config`, the wizard, or
// the profile struct. The value prioritises successful task
// completion; truncation is not a cost-control mechanism.
//
// 2026-09-14 final-simplification: lowered legacy user-set values
// (such as the alpha-era default of 1024) cannot truncate long
// synthesis work through user configuration. Pick a value large
// enough for the worst realistic consolidated memory (~64k input
// tokens + ~4k output tokens is comfortably under the cap).
const synthInternalMaxTokens = 16384

// InternalMaxTokens returns the substrate-supplied output-token
// ceiling.
//
// It exists so that every construction path for a SynthClient sources
// the limit from this one constant instead of re-deriving it. A caller
// that builds a SynthClient as a struct literal — rather than through
// NewSynthClient — has no way to reach the unexported constant, and
// the natural thing to reach for instead is the operator's
// Profiles[...].MaxTokens. That silently hands the budget to user
// configuration, which is exactly what synthInternalMaxTokens exists
// to prevent.
//
// Callers that need profile-driven connection fields (model, api key,
// base URL, timeout) should build the client with NewSynthClient and
// override those, or use this accessor for the token ceiling. Note that
// NewSynthClient also performs provider-specific env-var credential
// resolution, so a caller that must use the profile's api_key verbatim
// should prefer this accessor.
func InternalMaxTokens() int { return synthInternalMaxTokens }

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
// Synthesize sends memory fragments to the LLM using the consolidation
// prompt and returns the parsed synthesis result. Uses a context with
// the configured timeout for cancellation safety.
//
// Wire dispatch: Auth header, request path, and response parser are
// selected by sc.Wire (inferred from BaseURL at construction). Same
// body shape on both wires (system message first, user second in the
// messages array) — the divergence is in the URL, the header, and the
// attemptRepairIfFree consumes the recovery slot as RepairKind
// and re-issues via the wire helper in Recovery mode. The wire
// helper does NOT call AttemptFresh in recovery mode, so the
// recovery slot is consumed exactly once.
func (sc *SynthClient) attemptRepairIfFree(ctx context.Context, body map[string]interface{}, plan *Plan, fingerprint uint64) ([]byte, error) {
	if err := plan.AttemptRecovery(fingerprint, RecoveryRepair); err != nil {
		return nil, err
	}
	respBody, err := sc.DoLLMRequestWithPlan(ctx, body, plan, AttemptRecovery)
	if err != nil {
		return nil, err
	}
	return respBody, nil
}

// SynthesizeWithPlan is the canonical LLM call site for the
// synthesis stage. The Plan attributes every attempt the wire
// makes and is the authority for the "every model call has a
// finite pre-computable reason" invariant.
//
// Semantic dissatisfaction with the response (uncertain,
// ambiguous, contradictory, undefined, insufficient evidence)
// is NOT a retry reason — only mechanical failure is.
//
// 2026-09-14 tightening pass:
//   - Wire-envelope / structural-decode / empty-Content
//     failures consume the single recovery slot for
//     RepairKind.
//   - Empty Content is mechanical failure, NOT semantic
//     uncertainty.
func (sc *SynthClient) SynthesizeWithPlan(ctx context.Context, fragments []string, plan *Plan) (*SynthResult, error) {
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
	fingerprint := fingerprintFromBody(body)
	respBody, err := sc.DoLLMRequestWithPlan(ctx, body, plan, AttemptFresh)
	if err != nil {
		return nil, fmt.Errorf("synthesis [vendor=%s]: %w", wireLabel(sc.Wire), err)
	}
	rawResult, err := sc.ParseResponseBody(respBody, "synthesis")
	if err != nil {
		respBody2, err2 := sc.attemptRepairIfFree(ctx, body, plan, fingerprint)
		if err2 != nil {
			return nil, fmt.Errorf("synthesis [vendor=%s]: wire-envelope parse failed; repair: %w", wireLabel(sc.Wire), err2)
		}
		rawResult, err = sc.ParseResponseBody(respBody2, "synthesis")
		if err != nil {
			return nil, fmt.Errorf("synthesis [vendor=%s]: wire-envelope parse still fails on repair attempt: %w", wireLabel(sc.Wire), err)
		}
	}
	var result SynthResult
	if err := json.Unmarshal(rawResult, &result); err != nil {
		respBody2, err2 := sc.attemptRepairIfFree(ctx, body, plan, fingerprint)
		if err2 != nil {
			return nil, fmt.Errorf("failed to parse synthesis JSON; repair: %w", err2)
		}
		rawResult2, err := sc.ParseResponseBody(respBody2, "synthesis")
		if err != nil {
			return nil, fmt.Errorf("failed to parse synthesis JSON on repair attempt: %w", err)
		}
		if err := json.Unmarshal(rawResult2, &result); err != nil {
			return nil, fmt.Errorf("failed to parse synthesis JSON on repair attempt: %w", err)
		}
	}
	if result.Content == "" {
		respBody2, err2 := sc.attemptRepairIfFree(ctx, body, plan, fingerprint)
		if err2 != nil {
			return nil, fmt.Errorf("synthesis [vendor=%s]: empty provider response (mechanical failure); repair: %w", wireLabel(sc.Wire), err2)
		}
		rawResult2, err := sc.ParseResponseBody(respBody2, "synthesis")
		if err != nil {
			return nil, fmt.Errorf("synthesis [vendor=%s]: empty response on repair attempt: %w", wireLabel(sc.Wire), err)
		}
		if err := json.Unmarshal(rawResult2, &result); err != nil {
			return nil, fmt.Errorf("synthesis [vendor=%s]: empty response on repair attempt: %w", wireLabel(sc.Wire), err)
		}
		if result.Content == "" {
			return nil, fmt.Errorf("synthesis [vendor=%s]: empty provider response on repair attempt (twice); mechanical failure — stop", wireLabel(sc.Wire))
		}
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
