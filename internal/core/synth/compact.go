// compact.go — Strict-JSON synthesis for the compact_epistemology tool.
//
// compact_epistemology (see internal/core.CompactEpistemology) is the
// agent's reflex to the epistemic_pressure trigger. It needs the LLM
// to return a JSON object matching the CompactLesson schema
// ({title, body, tags}). Provenance is intentionally NOT in the
// LLM-facing schema — the parent package attaches it natively after
// unmarshal, eliminating the "lost in the middle" hallucination vector
// where the model could lose IDs in the response and strand raw
// memories in the pressure queue forever.
//
// This file adds SynthesizeCompactLesson to SynthClient. The general
// HTTP path mirrors the existing Synthesize method; the diff is the
// prompt and the return type (raw text string instead of *SynthResult).
// Returning the raw text keeps the JSON-schema enforcement at the
// orchestrator level, where domain validation lives.

package synth

import (
	"context"
	"fmt"
	"strings"
)

// compactLessonSystemPrompt is the strict-JSON instruction sent as the
// system message. Model returns ONLY the JSON — no preamble, no
// markdown fences, no explanation. The refusal sentinel
// {"title":"", "body":"", "tags":[]} is a valid response that the
// orchestrator treats as a schema refusal (empty fields trigger
// Validate() to fail).
const compactLessonSystemPrompt = `You are an epistemic consolidator. Given a batch of raw memories separated by "---MEMORY---" markers, synthesize a single durable lesson that captures their shared insight.

Return JSON matching this exact schema (no preamble, no markdown fences, no explanation):
{
  "title": "<short, descriptive title; <=80 chars>",
  "body":  "<the synthesized lesson in 1-3 paragraphs; markdown is OK>",
  "tags":  ["<kebab-case-topic-tag>", ...]
}

Rules:
- Title is a noun phrase, not a sentence.
- Body is durable knowledge, not specific facts from the batch.
- Tags are short kebab-case identifiers (e.g., "wakes-filter", "epistemic-compaction").
- If the batch is too noisy to synthesize, return {"title":"", "body":"", "tags":[]} — the orchestrator treats empty fields as a refusal.`

// SynthesizeCompactLesson sends the raw memories to the LLM with the
// strict-JSON compact prompt and returns the model's text response as
// a string. The orchestrator (internal/core.CompactEpistemology) is
// responsible for json.Unmarshal into its domain struct.
//
// Return contract: string is the raw LLM text. Caller unmarshals —
// unmarshal failures surface as model_schema_violation at the
// orchestrator level, where domain validation can inspect and the
// data plane can stay pristine (no DB writes on parse failure).
//
// HTTP transport: routes through doLLMRequest — wire-aware path
// selection (/messages vs /chat/completions) and auth header
// (X-Api-Key vs Authorization: Bearer) come from sc.Wire, which is
// inferred from BaseURL at construction time. Retry policy (one
// retry on 5xx with 3s backoff, fail-fast on 4xx) is centralized in
// the helper so all three call sites behave identically. Post-M3
// audit H-1: prior version hardcoded /messages + X-Api-Key which
// broke OpenRouter and OpenAI-protocol vendors.
func (sc *SynthClient) SynthesizeCompactLesson(ctx context.Context, rawMemories []string) (string, error) {
	if sc.APIKey == "" {
		return "", fmt.Errorf("no API key configured (set api_key in mpm_config.json synth block or appropriate env var for the configured wire)")
	}

	userContent := strings.Join(rawMemories, "\n---MEMORY---\n")

	body := map[string]interface{}{
		"model":      sc.Model,
		"max_tokens": sc.MaxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": compactLessonSystemPrompt},
			{"role": "user", "content": userContent},
		},
	}

	respBody, err := sc.DoLLMRequest(ctx, body)
	if err != nil {
		return "", fmt.Errorf("compact_lesson: %w", err)
	}

	rawResult, err := sc.ParseResponseBody(respBody, "compact_lesson")
	if err != nil {
		return "", fmt.Errorf("compact_lesson: %w", err)
	}
	return string(rawResult), nil
}