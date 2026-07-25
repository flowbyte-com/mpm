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
type SynthClient struct {
	Model     string
	APIKey    string
	BaseURL   string
	MaxTokens int
	Timeout   time.Duration
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
	if sc.APIKey == "" {
		if key := os.Getenv("MINIMAX_API_KEY"); key != "" {
			sc.APIKey = key
		}
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
func (sc *SynthClient) Synthesize(ctx context.Context, fragments []string) (*SynthResult, error) {
	if sc.APIKey == "" {
		return nil, fmt.Errorf("no API key configured (set api_key in mpm_config.json synth block or MINIMAX_API_KEY env var)")
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

	req, err := http.NewRequestWithContext(ctx, "POST", sc.BaseURL+"/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", sc.APIKey)

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

	rawResult, err := sc.ParseResponseBody(respBody, "default")
	if err != nil {
		return nil, fmt.Errorf("synthesis [vendor=default]: %w", err)
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

// ParseResponseBody parses the Anthropic-style response wrapper and returns
// the inner LLM output text as raw bytes. Exported because admission.go
// (different package) needs to call it on admission responses.
//
// The vendor parameter is included for contextual error messages — the caller
// passes the vendor name (or "default" / "admission" / etc.) so error logs
// identify which path produced the failure.
func (sc *SynthClient) ParseResponseBody(body []byte, vendor string) ([]byte, error) {
	wrapper := struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("vendor %s: failed to parse response wrapper: %w (body: %s)", vendor, err, string(body))
	}
	if len(wrapper.Content) == 0 {
		return nil, fmt.Errorf("vendor %s: API returned empty content", vendor)
	}
	// Find the first content block of type "text". Anthropic-style responses
	// may include a "thinking" block before the "text" block; using
	// content[0] would pick up the thinking block which has no Text field.
	var raw string
	for _, c := range wrapper.Content {
		if c.Type == "text" && c.Text != "" {
			raw = strings.TrimSpace(c.Text)
			break
		}
	}
	if raw == "" {
		return nil, fmt.Errorf("vendor %s: LLM returned empty response text (content types: %v)", vendor, contentTypes(wrapper.Content))
	}
	return []byte(raw), nil
}

func contentTypes(blocks []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Type)
	}
	return out
}
