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
//
// 2026-09-14 release-pass: the prompt permits uncertainty as a
// terminal outcome. Conflicting evidence, ambiguity, undefined
// proposition, or insufficient evidence are equally valid results;
// the model is instructed to preserve rather than force coherence.
// This is required for the bounded-execution safeguard — retry
// loops are bounded by the safeguard and never by the model
// re-reasoning on every "uncertain" response.
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

ACCEPTABLE TERMINAL OUTCOMES (any one is a successful synthesis):
- a coherent consolidated lesson
- a refusal sentinel {"title":"", "body":"", "tags":[]} when the
  batch is contradictory, ambiguous, mathematically undefined,
  or insufficiently specified. Preserve the refusal — do not
  invent a lesson just to fill the schema.
- an explicit body that names the contradiction and refuses to
  force a conclusion

You must NEVER fabricate values, soften contradictions, or
re-interpret opposing inputs as if one were correct just so the
JSON can be produced. Uncertainty is a valid result, not a
reason to keep calling.`

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
// Bounded-execution safeguard: SynthesizeCompactLesson builds a
// per-call Plan (one semantic call, default retry/repair budgets)
// and routes through DoLLMRequestWithPlan. Callers that need
// explicit attribution use SynthesizeCompactLessonWithPlan.
//
// HTTP transport: routes through DoLLMRequestWithPlan —
// wire-aware path selection (/messages vs /chat/completions)
// and auth header (X-Api-Key vs Authorization: Bearer) come from
// sc.Wire, which is inferred from BaseURL at construction time.
// Retry policy is now policy-driven (transient=1 retry, auth=0,
// rate-limit=conditional, malformed=1 repair). Post-M3 audit
// H-1: prior version hardcoded /messages + X-Api-Key which broke
// OpenRouter and OpenAI-protocol vendors.
func (sc *SynthClient) SynthesizeCompactLesson(ctx context.Context, rawMemories []string) (string, error) {
	return sc.SynthesizeCompactLessonWithPlan(ctx, rawMemories, NewPerCallPlan())
}

// SynthesizeCompactLessonWithPlan is the canonical LLM call
// site for the compact_epistemology lesson stage. The Plan
// attributes the call to the orchestrator's run budget.
//
// 2026-09-14 tightening pass: parse failures consume the
// recovery slot for RepairKind (single slot shared with
// transient retries). A second failure stops the run.
func (sc *SynthClient) SynthesizeCompactLessonWithPlan(ctx context.Context, rawMemories []string, plan *Plan) (string, error) {
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
	fingerprint := fingerprintFromBody(body)

	respBody, err := sc.DoLLMRequestWithPlan(ctx, body, plan, AttemptFresh)
	if err != nil {
		return "", fmt.Errorf("compact_lesson: %w", err)
	}

	rawResult, err := sc.ParseResponseBody(respBody, "compact_lesson")
	if err != nil {
		// Parse failure: allocate recovery slot as RepairKind
		// and re-issue in recovery mode (the wire helper does
		// NOT call AttemptFresh in recovery mode, so the
		// recovery slot is consumed exactly once).
		if errR := plan.AttemptRecovery(fingerprint, RecoveryRepair); errR != nil {
			return "", fmt.Errorf("compact_lesson parse failed; %w", errR)
		}
		respBody2, err2 := sc.DoLLMRequestWithPlan(ctx, body, plan, AttemptRecovery)
		if err2 != nil {
			return "", fmt.Errorf("compact_lesson repair: %w", err2)
		}
		rawResult, err = sc.ParseResponseBody(respBody2, "compact_lesson")
		if err != nil {
			return "", fmt.Errorf("compact_lesson repair parse still fails: %w", err)
		}
	}
	return string(rawResult), nil
}