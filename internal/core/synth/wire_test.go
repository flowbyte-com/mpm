package synth

import (
	"strings"
	"testing"
)

// TestInferWire_OpenRouter pins the URL → wire mapping for OpenRouter.
// Any URL containing "openrouter" should select wireOpenAI (which uses
// /chat/completions + Authorization: Bearer). This is the headline
// feature of this commit — getting it wrong means the bedrock
// capability is broken.
func TestInferWire_OpenRouter(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		expect wireShape
	}{
		{"vanilla openrouter", "https://openrouter.ai/api/v1", wireOpenAI},
		{"openrouter with trailing slash", "https://openrouter.ai/api/v1/", wireOpenAI},
		{"openrouter mixed case", "https://OpenRouter.AI/api/v1", wireOpenAI},
		{"openrouter in path only", "https://example.com/openrouter/api", wireOpenAI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inferWire(tc.url)
			if got != tc.expect {
				t.Errorf("inferWire(%q) = %v; want %v", tc.url, got, tc.expect)
			}
		})
	}
}

// TestInferWire_OpenAI covers native OpenAI, LM Studio (local
// OpenAI-compatible), and a few URL shapes that should still hit the
// OpenAI wire.
func TestInferWire_OpenAI(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		expect wireShape
	}{
		{"openai native", "https://api.openai.com/v1", wireOpenAI},
		{"lmstudio local", "http://localhost:1234/v1", wireOpenAI},
		{"lmstudio with 127.0.0.1", "http://127.0.0.1:1234/v1", wireOpenAI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inferWire(tc.url)
			if got != tc.expect {
				t.Errorf("inferWire(%q) = %v; want %v", tc.url, got, tc.expect)
			}
		})
	}
}

// TestInferWire_Anthropic pins the Anthropic-protocol default. Three
// cases that MUST stay on wireAnthropic for backward compatibility:
//   - MiniMax (default substrate vendor — Anthropic-shaped at /anthropic/v1)
//   - Anthropic native
//   - Plain Anthropic-protocol URL (custom deploys)
//   - Unknown / empty (default fallback must be Anthropic)
func TestInferWire_Anthropic(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		expect wireShape
	}{
		{"minimax default", "https://api.minimax.io/anthropic/v1", wireAnthropic},
		{"anthropic native", "https://api.anthropic.com", wireAnthropic},
		{"anthropic-style custom", "https://example.com/anthropic/v1", wireAnthropic},
		{"empty url defaults to anthropic", "", wireAnthropic},
		{"unknown vendor defaults to anthropic", "https://example.com/api/v1", wireAnthropic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inferWire(tc.url)
			if got != tc.expect {
				t.Errorf("inferWire(%q) = %v; want %v", tc.url, got, tc.expect)
			}
		})
	}
}

// TestWireOpenAI_AuthHeader pins Bearer token for OpenAI-protocol.
// This is the breaking case vs the prior hardcoded X-Api-Key.
func TestWireOpenAI_AuthHeader(t *testing.T) {
	name, value := wireOpenAI.authHeader("sk-or-test-key-1234")
	if name != "Authorization" {
		t.Errorf("OpenAI auth header name = %q; want Authorization", name)
	}
	want := "Bearer sk-or-test-key-1234"
	if value != want {
		t.Errorf("OpenAI auth header value = %q; want %q", value, want)
	}
}

// TestWireAnthropic_AuthHeader pins X-Api-Key for Anthropic-protocol.
// Regression guard — the prior hardcoded behaviour must remain.
func TestWireAnthropic_AuthHeader(t *testing.T) {
	name, value := wireAnthropic.authHeader("minimax-key-xyz")
	if name != "X-Api-Key" {
		t.Errorf("Anthropic auth header name = %q; want X-Api-Key", name)
	}
	if value != "minimax-key-xyz" {
		t.Errorf("Anthropic auth header value = %q; want minimax-key-xyz", value)
	}
}

// TestWirePath pins the URL path suffix per wire. The dispatch
// happens inside Synthesize via POST <baseURL>+path().
func TestWirePath(t *testing.T) {
	if got := wireOpenAI.path(); got != "/chat/completions" {
		t.Errorf("OpenAI path = %q; want /chat/completions", got)
	}
	if got := wireAnthropic.path(); got != "/messages" {
		t.Errorf("Anthropic path = %q; want /messages", got)
	}
}

// TestParseOpenAIResponse_Basic pins the response parser for the
// OpenAI-protocol wrapper. This is the success case — a 200 with
// a single choice containing the assistant message.
func TestParseOpenAIResponse_Basic(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-abc",
		"object": "chat.completion",
		"created": 1700000000,
		"model": "meta-llama/llama-3.3-70b-instruct",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "hello from llama"},
			"finish_reason": "stop"
		}]
	}`)
	got, err := parseOpenAIResponse(body)
	if err != nil {
		t.Fatalf("parseOpenAIResponse: %v", err)
	}
	if string(got) != "hello from llama" {
		t.Errorf("parseOpenAIResponse = %q; want %q", string(got), "hello from llama")
	}
}

// TestParseOpenAIResponse_TrimsWhitespace pins the trim behaviour.
// OpenRouter sometimes returns leading/trailing whitespace in
// content — the parser should strip it before returning.
func TestParseOpenAIResponse_TrimsWhitespace(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"  hello  \n"}}]}`)
	got, err := parseOpenAIResponse(body)
	if err != nil {
		t.Fatalf("parseOpenAIResponse: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("parseOpenAIResponse trim = %q; want %q", string(got), "hello")
	}
}

// TestParseOpenAIResponse_EmptyChoices pins the error path. An
// empty choices array is a malformed response — must surface as an
// error, not a silent empty string.
func TestParseOpenAIResponse_EmptyChoices(t *testing.T) {
	body := []byte(`{"choices":[]}`)
	_, err := parseOpenAIResponse(body)
	if err == nil {
		t.Errorf("parseOpenAIResponse: expected error on empty choices; got nil")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("parseOpenAIResponse error = %q; want it to mention 'no choices'", err.Error())
	}
}

// TestParseOpenAIResponse_Malformed pins the JSON-decode failure
// path. Non-JSON or truncated responses must surface a parse
// error, not a 0-byte result.
func TestParseOpenAIResponse_Malformed(t *testing.T) {
	body := []byte(`{this is not valid json`)
	_, err := parseOpenAIResponse(body)
	if err == nil {
		t.Errorf("parseOpenAIResponse: expected error on malformed JSON; got nil")
	}
}

// TestParseAnthropicResponse_Regression pins the Anthropic parser.
// Same shape returns same output as before this commit — the
// regression guard for the wire split.
func TestParseAnthropicResponse_Regression(t *testing.T) {
	body := []byte(`{
		"id": "msg_abc",
		"content": [
			{"type": "thinking", "text": ""},
			{"type": "text", "text": "  hello from claude  "}
		]
	}`)
	got, err := parseAnthropicResponse(body)
	if err != nil {
		t.Fatalf("parseAnthropicResponse: %v", err)
	}
	// Must pick the "text" block, NOT the "thinking" block.
	if string(got) != "hello from claude" {
		t.Errorf("parseAnthropicResponse = %q; want %q", string(got), "hello from claude")
	}
}

// TestWireDispatch pins that the wire dispatches via parseResponseBody
// — i.e. the SynthClient.* level dispatch goes through Wire. Pinned
// because the dispatcher table is now the only path; any future
// regression that hardcodes back into client.go is a one-line revert.
func TestWireDispatch(t *testing.T) {
	sc := &SynthClient{Wire: wireOpenAI}
	body := []byte(`{"choices":[{"message":{"content":"dispatcher-test"}}]}`)
	got, err := sc.Wire.parseResponseBody(body)
	if err != nil {
		t.Fatalf("OpenAI dispatch: %v", err)
	}
	if string(got) != "dispatcher-test" {
		t.Errorf("OpenAI dispatch = %q; want dispatcher-test", string(got))
	}

	sc.Wire = wireAnthropic
	body = []byte(`{"content":[{"type":"text","text":"anthropic-dispatch"}]}`)
	got, err = sc.Wire.parseResponseBody(body)
	if err != nil {
		t.Fatalf("Anthropic dispatch: %v", err)
	}
	if string(got) != "anthropic-dispatch" {
		t.Errorf("Anthropic dispatch = %q; want anthropic-dispatch", string(got))
	}
}

// TestParseResponseBody_ViaSynthClient pins that the exported method
// delegates through wire. admission.go and compact.go call this
// directly; they need to inherit the wire for free.
func TestParseResponseBody_ViaSynthClient(t *testing.T) {
	sc := &SynthClient{Wire: wireOpenAI}
	body := []byte(`{"choices":[{"message":{"content":"via-sc"}}]}`)
	got, err := sc.ParseResponseBody(body, "admission-test")
	if err != nil {
		t.Fatalf("ParseResponseBody: %v", err)
	}
	if string(got) != "via-sc" {
		t.Errorf("ParseResponseBody = %q; want via-sc", string(got))
	}
}
