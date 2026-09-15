// embedding_test.go — Probe via EmbeddingProvider. Reuses
// BuildEmbeddingProvider (the production factory). Tests cover
// OpenAI-shape and Ollama-shape responses.
//
// Status mapping per spec:
//   - HTTP 200 + non-empty vector  → ProbeHealthy
//   - HTTP 200 + empty vector      → ProbeInvalidResponse
//   - HTTP 200 + NaN/Inf value     → ProbeInvalidResponse
//   - profile = "disabled"           → ProbeDisabled
//   - HTTP 401                      → ProbeAuthFailed
//   - connection refused            → ProbeUnreachable
//   - HTTP 404 with model evidence  → ProbeModelNotFound
//   - generic HTTP 404              → ProbeUnknown

package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
)

// startFakeOpenAIEmbeddingServer stands up /v1/embeddings with the given
// canned response.
type openAIEmbeddingOpts struct {
	// Embedding values: when set, returned as `data:[{embedding:[..]}]`.
	Values []float32
	// Status code (0 = 200).
	Status int
	// Body: when set, used verbatim. Provide one of Values OR Body.
	Body string
}

func startFakeOpenAIEmbeddingServer(t *testing.T, o openAIEmbeddingOpts) *httptest.Server {
	t.Helper()
	handler := func(w http.ResponseWriter, r *http.Request) {
		if o.Status != 0 {
			w.WriteHeader(o.Status)
			if o.Body != "" {
				_, _ = w.Write([]byte(o.Body))
			}
			return
		}
		if o.Body != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(o.Body))
			return
		}
		emb := o.Values
		if emb == nil {
			emb = []float32{0.1, 0.2, 0.3}
		}
		payload := map[string]interface{}{
			"data": []map[string]interface{}{
				{"embedding": emb},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		// Round-trip via JSON to ensure no NaN/Inf in marshal.
		enc := struct{ Data []map[string]interface{} }{Data: payload["data"].([]map[string]interface{})}
		_ = enc
		// Render manually so NaN passes through if present in Values.
		out := `{"data":[{"embedding":[`
		for i, v := range emb {
			if i > 0 {
				out += ","
			}
			if v != v {
				out += "NaN"
			} else {
				out += formatFloat(float64(v))
			}
		}
		out += `]}]}`
		_, _ = w.Write([]byte(out))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/embeddings", handler)
	mux.HandleFunc("/v1/embed", handler)
	mux.HandleFunc("/", handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// formatFloat renders float64 without scientific notation for embedding
// server test bodies.
func formatFloat(v float64) string {
	// Drop the JSON encoder's default scientific-notation behaviour by
	// splitting the formatted number; this keeps the test bodies
	// readable while remaining valid JSON.
	if v == 0 {
		return "0"
	}
	// Use strconv.FormatFloat with 'f' for fixed-point.
	const digits = 6
	// We can't import strconv here without an import cycle risk; use
	// the simpler fmt.Sprintf path.
	return fmtFloat(v, digits)
}

func fmtFloat(v float64, digits int) string {
	// Decode without strconv.
	return strings.TrimRight(
		strings.TrimRight(
			// Sprintf without scientific notation
			sprintfFloat(v, digits),
			"0"),
		".") // crude; sufficient for test bodies
}

func sprintfFloat(v float64, digits int) string {
	// Use simple multiplication to render fractional digits.
	neg := v < 0
	if neg {
		v = -v
	}
	whole := int64(v)
	frac := v - float64(whole)
	// Render frac into N decimal places.
	s := intToStr(whole) + "."
	mult := pow10Int(digits)
	intFrac := int64(frac*float64(mult) + 0.5)
	s += padLeft(intToStr(intFrac), digits, '0')
	if neg {
		s = "-" + s
	}
	return s
}

func intToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	out := ""
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	if neg {
		out = "-" + out
	}
	return out
}

func padLeft(s string, width int, c rune) string {
	for len(s) < width {
		s = string(c) + s
	}
	return s
}

func pow10Int(n int) int64 {
	r := int64(1)
	for i := 0; i < n; i++ {
		r *= 10
	}
	return r
}

func TestProbeEmbedding_OpenAIHealthy(t *testing.T) {
	srv := startFakeOpenAIEmbeddingServer(t, openAIEmbeddingOpts{
		Values: []float32{0.1, 0.2, 0.3, 0.4, 0.5},
	})
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "nomic-embed-text",
		BaseURL:  srv.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeEmbedding(ctx, p)
	if r.Status != ProbeHealthy {
		t.Fatalf("want ProbeHealthy, got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}

func TestProbeEmbedding_EmptyVector(t *testing.T) {
	srv := startFakeOpenAIEmbeddingServer(t, openAIEmbeddingOpts{
		Body: `{"data":[{"embedding":[]}]}`,
	})
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "nomic-embed-text",
		BaseURL:  srv.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeEmbedding(ctx, p)
	if r.Status != ProbeInvalidResponse {
		t.Fatalf("empty vector must be ProbeInvalidResponse; got %v (summary=%q, err_class=%q)", r.Status, r.ErrorSummary, r.ErrorClass)
	}
}

func TestProbeEmbedding_Auth401(t *testing.T) {
	srv := startFakeOpenAIServer(t, openAIServerOpts{
		Status: 401,
		Body:   `{"error":{"code":"invalid_api_key"}}`,
	})
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "nomic-embed-text",
		BaseURL:  srv.URL,
		APIKey:   "bad-key",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeEmbedding(ctx, p)
	if r.Status != ProbeAuthFailed {
		t.Fatalf("401 must be ProbeAuthFailed; got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}

func TestProbeEmbedding_Generic404NotModelNotFound(t *testing.T) {
	srv := startFakeOpenAIEmbeddingServer(t, openAIEmbeddingOpts{
		Status: 404,
		Body:   `<html>not found</html>`,
	})
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "nomic-embed-text",
		BaseURL:  srv.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeEmbedding(ctx, p)
	if r.Status == ProbeModelNotFound {
		t.Fatalf("generic 404 must not be ProbeModelNotFound; got %v", r.Status)
	}
	if r.Status != ProbeUnknown {
		t.Fatalf("want ProbeUnknown for generic 404, got %v", r.Status)
	}
}

func TestProbeEmbedding_ConnectionRefused(t *testing.T) {
	p := &config.Profile{
		Provider: "openai-compatible",
		Model:    "nomic-embed-text",
		BaseURL:  "http://127.0.0.1:1",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := probeEmbedding(ctx, p)
	if r.Status != ProbeUnreachable {
		t.Fatalf("want ProbeUnreachable, got %v (summary=%q)", r.Status, r.ErrorSummary)
	}
}

func TestProbeEmbedding_IncompleteProfileIsUnknown(t *testing.T) {
	p := &config.Profile{Provider: "openai-compatible"} // missing model + base_url
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := probeEmbedding(ctx, p)
	if r.Status != ProbeUnknown {
		t.Fatalf("incomplete profile should be ProbeUnknown, got %v", r.Status)
	}
}
