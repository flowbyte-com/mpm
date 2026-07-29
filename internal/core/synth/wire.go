// Package synth — wire dispatch.
//
// LLM HTTP endpoints speak one of two well-known shapes today:
//   - Anthropic-protocol:  POST /messages              X-Api-Key header
//                          response: {"content":[{"type":"text","text":"..."}]}
//                          body:     messages array (system as first message)
//                          adopted by: Anthropic, MiniMax, LiteLLM-proxy-as-anthropic
//   - OpenAI-protocol:     POST /chat/completions     Authorization: Bearer header
//                          response: {"choices":[{"message":{"content":"..."}}]}
//                          body:     messages array (system as first message)
//                          adopted by: OpenAI, OpenRouter, LM Studio, Ollama-Compose
//
// Both protocols share the messages-array body shape, so the request body
// itself doesn't diverge — only the URL path, the auth header, and the
// response parser do. That's why this file is small.
//
// Wire selection is inferred from the base URL at client construction
// time. Inference is the same heuristic `inferProviderFromURL` uses
// (`internal/core/config/config.go`); the substrate doesn't store a
// wire/protocol field on Profile, so URL is the operative signal.
//
// Future: if a vendor's URL doesn't disambiguate cleanly, the Profile
// shape will gain a `wire` field — kept out for now to avoid premature
// schema surface.
package synth

import (
	"encoding/json"
	"fmt"
	"strings"
)

// wireShape names the HTTP wire the client will speak.
//
// New shapes would land here, not as scattered conditionals on
// `if URL contains X` inside client.go. Each shape owns:
//   - the URL path suffix (POST /messages vs POST /chat/completions)
//   - the auth header (X-Api-Key vs Authorization: Bearer)
//   - the response body parsing (Anthropic-style vs OpenAI-style)
type wireShape int

const (
	wireUnknown wireShape = iota
	wireAnthropic        // default — preserves prior single-shape behavior
	wireOpenAI           // OpenAI / OpenRouter / LM Studio / OpenAI-compatible locals
)

// inferWire returns the wire shape implied by a base URL. The heuristic
// covers the common cases operators will hit in practice:
//
//   - Hostname keywords: openrouter / openai / lmstudio / ollama
//   - Local-host shortcuts: localhost, 127.0.0.1
//   - LM Studio default port: 1234 (LM Studio's local listener)
//
// Every URL matching any of those routes to wireOpenAI — the OpenAI
// protocol — because the great majority of "I want to talk to a local
// LLM" or "I want a multi-model router" setups speak OpenAI-protocol.
// The substrate's historical default (wireAnthropic for URLs with
// "minimax", "anthropic", or no signal at all) is preserved for
// back-compat.
//
// Operators with a custom Anthropic-protocol endpoint on a generic
// localhost can disambiguate by including "anthropic" in the URL.
//
// Case-insensitive — URL casing varies across providers.
func inferWire(baseURL string) wireShape {
	lu := strings.ToLower(baseURL)
	switch {
	case strings.Contains(lu, "openrouter"),
		strings.Contains(lu, "openai.com"),
		strings.Contains(lu, "/openai/"),
		strings.HasSuffix(lu, "/openai"),
		strings.Contains(lu, "lmstudio"),
		strings.Contains(lu, "ollama") || strings.Contains(lu, ":11434"),
		strings.Contains(lu, "localhost"),
		strings.Contains(lu, "127.0.0.1"),
		strings.Contains(lu, ":1234"): // LM Studio default port
		return wireOpenAI
	}
	return wireAnthropic
}

// pathFor returns the URL path suffix to POST to on a given wire.
// Anthropic: /messages. OpenAI: /chat/completions.
func (w wireShape) path() string {
	switch w {
	case wireOpenAI:
		return "/chat/completions"
	default:
		return "/messages"
	}
}

// authHeader returns (headerName, headerValue) for a wire. Anthropic
// sends X-Api-Key with the key as value; OpenAI sends Authorization:
// Bearer <key>. The header is set verbatim — no escaping, since both
// protocols forbid CRLF in header values and our keys are short ASCII.
func (w wireShape) authHeader(apiKey string) (string, string) {
	switch w {
	case wireOpenAI:
		return "Authorization", "Bearer " + apiKey
	default:
		return "X-Api-Key", apiKey
	}
}

// parseResponseBody extracts the inner text from a vendor response body.
// Two shapes:
//
//   Anthropic-protocol:
//     {
//       "content": [{"type": "text", "text": "..."}]
//     }
//
//   OpenAI-protocol:
//     {
//       "choices": [{
//         "message": {"content": "..."},
//         ...
//       }]
//     }
//
// Both shapes are first-class. The dispatcher here is a small switch;
// if a third wire ever lands, the table grows.
//
// Returns the inner text bytes (trimmed) for the calling SynthClient
// to JSON-unmarshal into a SynthResult. Errors carry the wire name
// for diagnostically-useful watchdog messages.
func (w wireShape) parseResponseBody(body []byte) ([]byte, error) {
	switch w {
	case wireOpenAI:
		return parseOpenAIResponse(body)
	default:
		return parseAnthropicResponse(body)
	}
}

// parseAnthropicResponse reads the Anthropic-protocol wrapper and
// returns the first "text" content block's text (trimmed). The
// Anthropic wire may include a "thinking" block before the "text"
// block; using content[0] would pick up the thinking block, which
// has no Text field — we iterate and break on the first text block.
func parseAnthropicResponse(body []byte) ([]byte, error) {
	wrapper := struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("anthropic wire: failed to parse response wrapper: %w (body: %s)", err, string(body))
	}
	if len(wrapper.Content) == 0 {
		return nil, fmt.Errorf("anthropic wire: API returned empty content")
	}
	var raw string
	for _, c := range wrapper.Content {
		if c.Type == "text" && c.Text != "" {
			raw = strings.TrimSpace(c.Text)
			break
		}
	}
	if raw == "" {
		return nil, fmt.Errorf("anthropic wire: LLM returned empty response text (content types: %v)", contentTypes(wrapper.Content))
	}
	return []byte(raw), nil
}

// parseOpenAIResponse reads the OpenAI-protocol wrapper and returns
// the first choice's message content (trimmed). OpenRouter and other
// OpenAI-compatible providers follow this shape; the `choices` array
// is len-1 in non-streaming mode.
//
// Note: stream=false (the substrate's only mode today) means we don't
// have to handle SSE incremental chunks. If streaming is ever added,
// this would split into two paths.
func parseOpenAIResponse(body []byte) ([]byte, error) {
	wrapper := struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}{}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("openai wire: failed to parse response wrapper: %w (body: %s)", err, string(body))
	}
	if len(wrapper.Choices) == 0 {
		return nil, fmt.Errorf("openai wire: API returned no choices")
	}
	content := strings.TrimSpace(wrapper.Choices[0].Message.Content)
	if content == "" {
		return nil, fmt.Errorf("openai wire: LLM returned empty message content")
	}
	return []byte(content), nil
}

// contentTypes is a tiny helper used in Anthropic error messages.
// Kept here (instead of inlined where it was previously) so the wire
// file owns all Anthropic-protocol response concerns.
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
