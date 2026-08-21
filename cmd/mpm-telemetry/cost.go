package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/flowbyte-com/mpm/internal/telemetry"
)

func runCost(args []string) error {
	fs := flag.NewFlagSet("cost", flag.ContinueOnError)
	pricingPath := fs.String("pricing", "", "path to pricing catalog JSON (required)")
	sinceCutoff := fs.Int64("since", 0, "Unix epoch seconds; only count rows newer than this")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pricingPath == "" {
		return fmt.Errorf("--pricing is required")
	}
	catalog, err := telemetry.LoadPricingCatalog(*pricingPath)
	if err != nil {
		return err
	}

	store, err := openDefaultStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := store.QuerySince(ctx, *sinceCutoff)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	cost, err := telemetry.ProjectCost(rows, catalog)
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(map[string]any{
		"since":             *sinceCutoff,
		"row_count":         len(rows),
		"cost_by_invocation": cost,
		"total":             sumValues(cost),
	}, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

func sumValues(m map[string]float64) float64 {
	var s float64
	for _, v := range m {
		s += v
	}
	return s
}
