// do_request_test.go — adversarial coverage for DoLLMRequest.
//
// First httptest use in the synth package. The HTTP path is the most
// expensive surface to exercise against real vendors; until now the
// existing wire_test.go only verified pure parsers and the dispatch
// table, not the actual request shape (path + headers) sent to the
// LLM. Post-M3 audit H-1 specifically called out that compact.go and
// admission.go hardcoded /messages + X-Api-Key; the regression guard
// is a test that asserts the wire dispatch makes it through.
package synth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestDoLLMRequest_PathAndHeaderPerWire is the core regression guard:
// every wire dispatches the right URL path and the right auth header.
// A single httptest server stands in for the vendor and captures both.
// If anyone re-introduces a hardcoded "/messages" or "X-Api-Key" in
// any of the three call sites, this test catches it at the wire
// boundary rather than at the live vendor 401.
func TestDoLLMRequest_PathAndHeaderPerWire(t *testing.T) {
	cases := []struct {
		name           string
		wire           wireShape
		baseURL        string
		apiKey         string
		wantPath       string
		wantAuthName   string
		wantAuthValue  string
		wantBodyShape  string // a key the body must contain
		responseBody   string
	}{
		{
			name:          "anthropic_wire",
			wire:          wireAnthropic,
			baseURL:       "https://api.anthropic.com/v1",
			apiKey:        "sk-test-anthropic",
			wantPath:      "/messages",
			wantAuthName:  "X-Api-Key",
			wantAuthValue: "sk-test-anthropic",
			wantBodyShape: "model",
			responseBody:  `{"content":[{"type":"text","text":"anthropic-ok"}]}`,
		},
		{
			name:          "openai_wire",
			wire:          wireOpenAI,
			baseURL:       "https://api.openai.com/v1",
			apiKey:        "sk-test-openai",
			wantPath:      "/chat/completions",
			wantAuthName:  "Authorization",
			wantAuthValue: "Bearer sk-test-openai",
			wantBodyShape: "model",
			responseBody:  `{"choices":[{"message":{"content":"openai-ok"}}]}`,
		},
		{
			name:          "openrouter_wire",
			wire:          wireOpenAI,
			baseURL:       "https://openrouter.ai/api/v1",
			apiKey:        "sk-test-openrouter",
			wantPath:      "/chat/completions",
			wantAuthName:  "Authorization",
			wantAuthValue: "Bearer sk-test-openrouter",
			wantBodyShape: "model",
			responseBody:  `{"choices":[{"message":{"content":"or-ok"}}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotAuthName, gotAuthValue, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuthName, gotAuthValue = "", ""
				// OpenAI sends "Authorization"; Anthropic sends "X-Api-Key".
				if v := r.Header.Get("Authorization"); v != "" {
					gotAuthName = "Authorization"
					gotAuthValue = v
				}
				if v := r.Header.Get("X-Api-Key"); v != "" {
					gotAuthName = "X-Api-Key"
					gotAuthValue = v
				}
				buf := make([]byte, r.ContentLength)
				if r.ContentLength > 0 {
					_, _ = r.Body.Read(buf)
				}
				gotBody = string(buf)
				w.WriteHeader(200)
				_, _ = w.Write([]byte(tc.responseBody))
			}))
			defer srv.Close()

			sc := &SynthClient{
				BaseURL: srv.URL, // httptest URL — path() suffix is the test target
				APIKey:  tc.apiKey,
				Wire:    tc.wire,
				Timeout: 5 * time.Second,
			}
			body, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{
				"model": "x",
				"messages": []map[string]string{
					{"role": "user", "content": "hi"},
				},
			})
			if err != nil {
				t.Fatalf("DoLLMRequest: %v", err)
			}
			if gotPath != tc.wantPath {
				t.Errorf("path = %q, want %q (H-1: wire dispatch must select /messages or /chat/completions)", gotPath, tc.wantPath)
			}
			if gotAuthName != tc.wantAuthName {
				t.Errorf("auth name = %q, want %q (H-1: wire dispatch must select X-Api-Key or Authorization)", gotAuthName, tc.wantAuthName)
			}
			if gotAuthValue != tc.wantAuthValue {
				t.Errorf("auth value = %q, want %q", gotAuthValue, tc.wantAuthValue)
			}
			if !strings.Contains(gotBody, tc.wantBodyShape) {
				t.Errorf("body missing %q: got %q", tc.wantBodyShape, gotBody)
			}
			if string(body) != tc.responseBody {
				t.Errorf("response body = %q, want %q", string(body), tc.responseBody)
			}
		})
	}
}

// TestDoLLMRequest_5xxRetries pins the retry contract: one retry on 5xx,
// ctx-aware backoff, no retry on 4xx. This is the parity guarantee the
// admission path lacked before this commit (single attempt).
func TestDoLLMRequest_5xxRetries(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"after-retry"}]}`))
	}))
	defer srv.Close()

	sc := &SynthClient{
		BaseURL: srv.URL,
		APIKey:  "k",
		Wire:    wireAnthropic,
		Timeout: 5 * time.Second,
	}
	// Tight ctx so the retry doesn't actually wait 3s; 5s covers it but
	// keeps test runtime bounded.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, err := sc.DoLLMRequest(ctx, map[string]interface{}{"model": "x"})
	if err != nil {
		t.Fatalf("DoLLMRequest: %v (expected success after 1 retry)", err)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if !strings.Contains(string(body), "after-retry") {
		t.Errorf("body = %q, want after-retry", string(body))
	}
}

// TestDoLLMRequest_4xxFailsFast pins the 4xx contract: no retry, error
// surfaces immediately. Without this guard a malformed payload could
// retry forever and amplify operator cost.
func TestDoLLMRequest_4xxFailsFast(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer srv.Close()

	sc := &SynthClient{
		BaseURL: srv.URL,
		APIKey:  "k",
		Wire:    wireAnthropic,
		Timeout: 5 * time.Second,
	}
	_, err := sc.DoLLMRequest(context.Background(), map[string]interface{}{"model": "x"})
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if atomic.LoadInt32(&attempts) != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 4xx)", attempts)
	}
}
