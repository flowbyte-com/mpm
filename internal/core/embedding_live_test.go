//go:build live_ollama

package internal

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

// TestLiveOllamaEmbedding exercises the full Ollama embedding round-trip
// against a real endpoint. It is gated on the live_ollama build tag and
// skips cleanly when OLLAMA_ENDPOINT is unset or the endpoint is unreachable.
func TestLiveOllamaEmbedding(t *testing.T) {
	endpoint := os.Getenv("OLLAMA_ENDPOINT")
	if endpoint == "" {
		t.Skip("OLLAMA_ENDPOINT not set; skipping live Ollama test")
	}

	model := os.Getenv("OLLAMA_MODEL")
	// Probe the endpoint to discover the actual embedding dimension.
	dim, err := probeOllamaDimension(endpoint, model)
	if err != nil {
		t.Skipf("Ollama endpoint unreachable (%s); skipping live test: %v", endpoint, err)
	}

	// Round-trip a known content string.
	const content = "hello world"
	vec, err := EmbedText(content)
	if err != nil {
		t.Fatalf("EmbedText(%q) returned error: %v", content, err)
	}
	if vec == nil {
		t.Fatal("EmbedText returned nil vector")
	}

	// Dimension must match what the probe discovered.
	if len(vec) != dim {
		t.Errorf("embedding dimension = %d, want %d (model %q)", len(vec), dim, model)
	}

	// Sanity check: at least one non-zero element.
	allZero := true
	for _, v := range vec {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("embedding vector is all zeros — response was likely not parsed correctly")
	}
}

// probeOllamaDimension makes a minimal API call to discover the embedding
// dimension for the given model without depending on a hard-coded model map.
func probeOllamaDimension(endpoint, model string) (int, error) {
	if model == "" {
		model = "nomic-embed-text"
	}
	reqBody := map[string]interface{}{
		"model":  model,
		"prompt": "dimension-probe",
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return 0, err
	}
	client := &http.Client{}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, err
	}
	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}
	return len(result.Embedding), nil
}
