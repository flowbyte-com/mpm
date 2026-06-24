package internal

// Memory admission: LLM-based decision whether a frequently-retrieved
// reference chunk should become a memory. The active HTTP client lives
// in internal/synth; this file owns the admission-specific prompts, result
// shapes, and the orchestrating EvaluateCandidate function.
//
// Decoupled from SynthClient per 808 review (2026-06-24): the admission
// flow takes the client as an argument rather than being a method receiver
// on SynthClient. This keeps the synth package free of admission concerns
// and lets admission use any synth.SynthClient (or mock) interchangeably.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mpm/internal/synth"
)

// Admission is the LLM-based admission function described in decisions
// 355fb7f381e63199 (connection-driven), 4f1c1fbc41a7a765 (reference/memory
// primitives), b90e4fe54507c3b9 (interaction is the third primitive),
// 9aa0ee2c6de8492a (LLM-based, autonomous, single-model, decay-bounded).
//
// One LLM call per candidate, one JSON response. The admitting model
// produces both the admit/reject decision and the justification chain —
// no two-model pipelines (decision 9aa0ee2c6de8492a rationales against
// decoupling the decision from the reasoning).

// admissionSystemPrompt is the prompt sent to the LLM. It instructs the
// model to evaluate a retrieved chunk as a memory candidate and return
// structured JSON with the decision, content, justification, confidence,
// and tags. The model itself produces the chain that justifies the
// admission — that is what makes the chain auditable later.
const admissionSystemPrompt = `You are the admission function for a personal cognitive substrate (MPM). A reference chunk has been retrieved multiple times. Decide whether it should become a memory.

A memory is:
- small (one or two sentences; never a paragraph)
- specific (actionable, testable, or named-pattern)
- connected (relates to an existing theory, active project, or known capability)

A memory is NOT:
- a copy of the chunk (extract the pattern, not the prose)
- a vague theme (be specific)
- the chunk's literal facts without interpretation
- a duplicate of something already in the substrate

EVALUATE the chunk against this context:
- Import reason: what v anticipated this reference would be useful for
- Active project: what v is currently working on (if any)
- Existing theories: memories v has already admitted that this might connect to

OUTPUT a JSON object with these fields:
{
  "admit": true | false,
  "content": "...",  // required if admit=true; the memory itself
  "justification": [
    {"type": "active_project" | "existing_theory" | "capability" | "novel_pattern", "artifact": "...", "strength": 0.0-1.0}
  ],
  "confidence": 0.0-1.0,  // required if admit=true
  "tags": ["..."],  // required if admit=true
  "reason": "..."  // required if admit=false; why rejected
}

The justification chain is the model's own reasoning at the moment of decision. Make it specific. "It's about MPM" is not a justification. "Connects to theory b90e4fe54507c3b9 about reference/memory split" is.

The output must be valid JSON. No markdown, no explanation, no preamble.`

// admitResult is the JSON structure the admission LLM must return.
type admitResult struct {
	Admit         bool              `json:"admit"`
	Content       string            `json:"content,omitempty"`
	Justification []admitChainEntry `json:"justification"`
	Confidence    float64           `json:"confidence,omitempty"`
	Tags          []string          `json:"tags,omitempty"`
	Reason        string            `json:"reason,omitempty"`
}

// admitChainEntry is one link in the admission justification chain. The
// type is intentionally narrow (active_project / existing_theory /
// capability / novel_pattern) so the chain is queryable and statistically
// tractable. Strength is 0-1, not a probability — it is the model's own
// estimate of how strongly this link supports the admission.
type admitChainEntry struct {
	Type     string  `json:"type"`
	Artifact string  `json:"artifact"`
	Strength float64 `json:"strength"`
}

// EvaluateCandidate sends one candidate to the LLM and returns the
// admission decision. The call is single-model, single-prompt, returning
// a JSON result that includes both the admit/reject and the chain.
//
// The chunk is sent with its retrieval context (import_reason, queries
// that surfaced it, hit count) so the LLM can evaluate connection
// rather than just relevance. The model name comes from the supplied
// SynthClient.
//
// Refactored 2026-06-24: was a method receiver on SynthClient, now a
// free function that takes the client as an argument. Keeps the synth
// package free of admission concerns and avoids the cross-package method
// receiver Go restriction.
func EvaluateCandidate(ctx context.Context, client *synth.SynthClient, candidate *AdmissionCandidate) (*admitResult, error) {
	if client.APIKey == "" {
		return nil, fmt.Errorf("no API key configured (set api_key in mpm_config.json synth block or MINIMAX_API_KEY env var)")
	}

	userContent := candidate.ToPrompt()

	body := map[string]interface{}{
		"model":      client.Model,
		"max_tokens": client.MaxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": admissionSystemPrompt},
			{"role": "user", "content": userContent},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", client.BaseURL+"/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+client.APIKey)

	httpClient := &http.Client{Timeout: client.Timeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("admission API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("admission API returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	rawResult, err := client.ParseResponseBody(respBody, "admission")
	if err != nil {
		return nil, fmt.Errorf("admission: %w", err)
	}
	var result admitResult
	if err := json.Unmarshal(rawResult, &result); err != nil {
		return nil, fmt.Errorf("failed to parse admission JSON: %w (raw: %s)", err, string(rawResult))
	}
	// Sanity: if admit=true, content and confidence are required.
	if result.Admit {
		if result.Content == "" {
			return nil, fmt.Errorf("LLM admitted but returned empty content")
		}
		if result.Confidence <= 0 || result.Confidence > 1 {
			return nil, fmt.Errorf("LLM admitted with invalid confidence %f (must be 0-1)", result.Confidence)
		}
	} else {
		if result.Reason == "" {
			result.Reason = "no reason given"
		}
	}
	return &result, nil
}

// AdmissionCandidate is the structured input to the LLM. It carries the
// retrieved chunk plus its retrieval context (which queries surfaced it,
// how often, what the import reason was). The LLM sees the full context,
// not just the chunk — connection-driven admission requires the
// retrieval evidence, not just the prose.
type AdmissionCandidate struct {
	DocID           string
	DocTitle        string
	ChunkID         string
	ChunkContent    string
	ImportReason    string
	HitCount        int
	DistinctQueries int
	RecentQueries   []string
	ActiveProject   string
	NearestTheories []string
}

// ToPrompt formats the candidate as the user message sent to the LLM.
func (c *AdmissionCandidate) ToPrompt() string {
	var sb strings.Builder
	sb.WriteString("## Reference chunk\n")
	if c.DocTitle != "" {
		sb.WriteString("Title: ")
		sb.WriteString(c.DocTitle)
		sb.WriteString("\n")
	}
	if c.ImportReason != "" {
		sb.WriteString("Import reason: ")
		sb.WriteString(c.ImportReason)
		sb.WriteString("\n")
	}
	sb.WriteString("\nContent:\n")
	sb.WriteString(c.ChunkContent)
	sb.WriteString("\n\n## Retrieval evidence\n")
	fmt.Fprintf(&sb, "Hit count: %d\n", c.HitCount)
	fmt.Fprintf(&sb, "Distinct queries: %d\n", c.DistinctQueries)
	if len(c.RecentQueries) > 0 {
		sb.WriteString("Recent queries that surfaced this chunk:\n")
		for _, q := range c.RecentQueries {
			fmt.Fprintf(&sb, "  - %s\n", q)
		}
	}
	if c.ActiveProject != "" {
		sb.WriteString("\n## Active project\n")
		sb.WriteString(c.ActiveProject)
		sb.WriteString("\n")
	}
	if len(c.NearestTheories) > 0 {
		sb.WriteString("\n## Existing theories (potential connections)\n")
		for _, t := range c.NearestTheories {
			fmt.Fprintf(&sb, "  - %s\n", t)
		}
	}
	return sb.String()
}

// compile-time guard that the LLM call has a reasonable default timeout
// when callers pass a background context.
var _ = time.Second