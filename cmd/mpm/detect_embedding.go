// cmd/mpm/detect_embedding.go — `mpm config detect-embedding`
//
// Operator-driven diagnostic. Probes localhost:11434 by default;
// never mutates configuration unless --apply is supplied.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// DetectEmbeddingCmd probes for Ollama and optionally writes a profile.
type DetectEmbeddingCmd struct {
	// Apply is the profile name to create and bind when --apply is given.
	Apply string
	// Force overwrites an existing profile and rebinds components.embedding
	// when --apply is given. Without --force, --apply refuses to mutate
	// any existing profile.
	Force bool
}

// Run executes the detect-embedding command.
func (c *DetectEmbeddingCmd) Run() int {
	endpoint := os.Getenv("OLLAMA_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	models, reachable := probeOllamaModels(endpoint)

	fmt.Println("Embedding provider detection")
	fmt.Println()
	fmt.Println("Ollama")
	fmt.Printf("  %s\n", endpoint)
	if reachable {
		fmt.Println("  reachable: yes")
		if len(models) == 0 {
			fmt.Println("  models: (none reported)")
		} else {
			fmt.Println("  models:")
			for _, m := range models {
				fmt.Printf("    %s\n", m)
			}
		}
	} else {
		fmt.Println("  reachable: no")
	}
	fmt.Println()

	if c.Apply == "" {
		fmt.Println("No configuration has been changed.")
		return 0
	}

	// --apply: write profile + components.embedding binding.
	if !reachable || len(models) == 0 {
		usererror.Error("--apply refused: no reachable Ollama endpoint with embedding-capable models")
		return 1
	}

	// Pick the lexicographically smallest model name — deterministic and visible.
	sorted := make([]string, len(models))
	copy(sorted, models)
	sort.Strings(sorted)
	model := sorted[0]

	cfg, err := config.LoadConfig()
	if err != nil {
		usererror.Error("--apply: load config: %v", err)
		return 1
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]config.Profile{}
	}
	if _, exists := cfg.Profiles[c.Apply]; exists && !c.Force {
		usererror.Errorf(fmt.Sprintf("--apply refused: profile %q already exists; re-run with --force to overwrite, or `mpm config profile remove %s` first", c.Apply, c.Apply))
		return 1
	}
	// --force on an existing binding: warn so the operator knows they
	// are about to rebind components.embedding.
	if existingBinding, hasBinding := cfg.Components["embedding"]; hasBinding && existingBinding != c.Apply {
		if c.Force {
			fmt.Printf("⚠  rebinding components[\"embedding\"]: %q → %q (--force)\n", existingBinding, c.Apply)
		}
	}
	cfg.Profiles[c.Apply] = config.Profile{
		Provider: "ollama",
		Model:    model,
		BaseURL:  endpoint,
	}
	if cfg.Components == nil {
		cfg.Components = map[string]string{}
	}
	cfg.Components["embedding"] = c.Apply
	if err := config.SaveConfig(cfg); err != nil {
		usererror.Error("--apply: save config: %v", err)
		return 1
	}
	fmt.Printf("Wrote profile %q (provider=ollama, model=%s).\n", c.Apply, model)
	fmt.Printf("Set components[\"embedding\"] = %q.\n", c.Apply)
	return 0
}

// probeOllamaModels hits the /api/tags endpoint and returns discovered model names.
func probeOllamaModels(endpoint string) ([]string, bool) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(endpoint + "/api/tags")
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&out); err != nil {
		return nil, false
	}
	var names []string
	for _, m := range out.Models {
		names = append(names, m.Name)
	}
	return names, true
}
