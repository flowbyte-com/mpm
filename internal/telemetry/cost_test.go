package telemetry

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectCost_AppliesCatalog(t *testing.T) {
	s := newStore(t)
	f := sampleFrame()
	f.InputTokens = ptrInt64(1000)
	f.OutputTokens = ptrInt64(200)
	f.CacheReadTokens = ptrInt64(500)
	f.CacheWriteTokens = ptrInt64(0)
	f.ReasoningTokens = ptrInt64(100)
	if _, err := s.InsertFrame(context.Background(), f); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := s.QuerySince(context.Background(), 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	cost, err := ProjectCost(rows, []Pricing{{
		Provider: "anthropic", Model: "claude-fable-5",
		InputPer1k: 3.0, OutputPer1k: 15.0,
		CacheReadPer1k: 0.3, CacheWritePer1k: 3.75, ReasoningPer1k: 15.0,
	}})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	got := cost[f.InvocationID]
	// input 1000 * 3.0/1k = 3.0
	// output 200 * 15.0/1k = 3.0
	// cache_read 500 * 0.3/1k = 0.15
	// cache_write 0 * 3.75/1k = 0
	// reasoning 100 * 15.0/1k = 1.5
	// total = 7.65
	if got < 7.64 || got > 7.66 {
		t.Errorf("cost = %v, want ~7.65", got)
	}
}

func TestProjectCost_UnknownProviderFails(t *testing.T) {
	s := newStore(t)
	f := sampleFrame()
	if _, err := s.InsertFrame(context.Background(), f); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := s.QuerySince(context.Background(), 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	_, err = ProjectCost(rows, []Pricing{}) // empty catalog
	if err == nil {
		t.Fatalf("expected unknown-pricing error")
	}
	if !strings.Contains(err.Error(), "unknown_pricing") {
		t.Errorf("error = %v, want unknown_pricing", err)
	}
}

func TestLoadPricingCatalog(t *testing.T) {
	catalog, err := LoadPricingCatalog(filepath.Join("testdata", "one-model.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(catalog) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(catalog))
	}
	if catalog[0].Model != "claude-fable-5" {
		t.Errorf("Model = %q", catalog[0].Model)
	}
}
