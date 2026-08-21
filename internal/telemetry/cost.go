// internal/telemetry/cost.go — read-time pricing projection.
//
// Per spec §9 (deferred-pricing): pricing is external and applied at
// read time. The raw ledger stores provider-reported token counters
// only. ProjectCost maps a set of Frames to a per-invocation dollar
// estimate using a versioned Pricing catalog.

package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
)

type Pricing struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	InputPer1k       float64 `json:"input_per_1k"`
	OutputPer1k      float64 `json:"output_per_1k"`
	CacheReadPer1k   float64 `json:"cache_read_per_1k"`
	CacheWritePer1k  float64 `json:"cache_write_per_1k"`
	ReasoningPer1k   float64 `json:"reasoning_per_1k"`
	EffectiveFrom    string  `json:"effective_from"`
	EffectiveTo      string  `json:"effective_to"`
}

type pricingCatalogFile struct {
	Entries []Pricing `json:"entries"`
}

func LoadPricingCatalog(path string) ([]Pricing, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	var f pricingCatalogFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	return f.Entries, nil
}

func ProjectCost(rows []Frame, catalog []Pricing) (map[string]float64, error) {
	out := make(map[string]float64, len(rows))
	for _, f := range rows {
		var p *Pricing
		for i := range catalog {
			if catalog[i].Provider == f.Provider && catalog[i].Model == f.Model {
				p = &catalog[i]
				break
			}
		}
		if p == nil {
			return nil, fmt.Errorf("unknown_pricing: %s/%s (invocation %s)", f.Provider, f.Model, f.InvocationID)
		}
		var cost float64
		cost += tokensCost(f.InputTokens, p.InputPer1k)
		cost += tokensCost(f.OutputTokens, p.OutputPer1k)
		cost += tokensCost(f.CacheReadTokens, p.CacheReadPer1k)
		cost += tokensCost(f.CacheWriteTokens, p.CacheWritePer1k)
		cost += tokensCost(f.ReasoningTokens, p.ReasoningPer1k)
		out[f.InvocationID] = cost
	}
	return out, nil
}

func tokensCost(n *int64, per1k float64) float64 {
	if n == nil {
		return 0
	}
	return float64(*n) / 1000.0 * per1k
}
