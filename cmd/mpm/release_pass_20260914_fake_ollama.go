// release_pass_20260914_fake_ollama.go — In-process Ollama test
// server used by the role-validation regression tests.
//
// Stands up a tiny HTTP server that speaks the Ollama /api/show
// and /api/tags endpoints so ProbeOllamaCapabilities can be
// driven hermetically without depending on the operator's
// real Ollama installation.
//
// Capability metadata is keyed by model name (case-insensitive
// substring match in the show probe; exact match in the tags
// listing). Empty maps mean "probe returns no metadata" — the
// probe falls back to the secondary name list.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOllamaOpts configures the in-process Ollama test server.
// Both ShowCapabilities and TagsCapabilities are checked:
// /api/show is tried first, then /api/tags is the fallback.
type fakeOllamaOpts struct {
	ShowCapabilities map[string][]string
	TagsCapabilities map[string][]string
}

// startFakeOllama stands up a fake Ollama server with the
// given capability metadata. The returned *httptest.Server
// exposes its URL via the URL field; tests should defer
// srv.Close().
func startFakeOllama(t *testing.T, opts fakeOllamaOpts) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		name := strings.ToLower(req.Name)
		var caps []string
		for k, v := range opts.ShowCapabilities {
			if strings.EqualFold(k, name) || strings.EqualFold(k+":latest", name) {
				caps = v
				break
			}
		}
		// 200 with empty capabilities means "probe returned,
		// no metadata". Return 200 + empty array so the
		// probe sees FromAPI=false (both flags zero).
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"capabilities": caps,
		})
	})

	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		var models []map[string]interface{}
		for name, caps := range opts.TagsCapabilities {
			models = append(models, map[string]interface{}{
				"name": name,
				"details": map[string]interface{}{
					"capabilities": caps,
				},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models": models,
		})
	})

	return httptest.NewServer(mux)
}
