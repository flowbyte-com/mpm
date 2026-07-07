package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func parseOpsConfidenceArgs(args []string) (map[string]interface{}, string, error) {
	if len(args) == 0 {
		return nil, "", fmt.Errorf("subcommand required: show|recompute|changes|trend")
	}
	sub := args[0]
	switch sub {
	case "show", "recompute", "changes", "trend":
	default:
		return nil, "", fmt.Errorf("unknown subcommand: %q (want show|recompute|changes|trend)", sub)
	}
	fs := flag.NewFlagSet("ops-confidence-"+sub, flag.ContinueOnError)
	artifactID := fs.String("artifact", "", "artifact id (required)")
	artifactType := fs.String("artifact-type", "memory", "artifact type")
	limit := fs.Int("limit", 10, "history rows to show (show only)")
	windowDays := fs.Int("window-days", 30, "trend window in days (trend only)")
	if err := fs.Parse(args[1:]); err != nil {
		return nil, "", err
	}
	if *artifactID == "" {
		return nil, "", fmt.Errorf("--artifact is required")
	}
	payload := map[string]interface{}{
		"artifact_id":   *artifactID,
		"artifact_type": *artifactType,
		"limit":         *limit,
		"window_days":   *windowDays,
	}
	return payload, sub, nil
}

func handleOpsConfidence(args []string) int {
	payload, sub, err := parseOpsConfidenceArgs(args)
	if err != nil {
		printError("%v", err)
		return 1
	}
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		printError("open database: %v", err)
		return 1
	}
	defer dm.Close()

	artifactID, _ := payload["artifact_id"].(string)
	artifactType, _ := payload["artifact_type"].(string)

	var res interface{}
	switch sub {
	case "show":
		limit, _ := payload["limit"].(int)
		snap, err := mpminternal.GetConfidenceForArtifact(dm, artifactID, artifactType, limit)
		if err != nil {
			printError("ops confidence show: %v", err)
			return 1
		}
		res = snap
	case "recompute":
		if err := mpminternal.RecomputeConfidence(dm, artifactID, artifactType, mpminternal.RecomputeReasonManual); err != nil {
			printError("ops confidence recompute: %v", err)
			return 1
		}
		res = map[string]interface{}{"success": true, "artifact_id": artifactID}
	case "changes":
		limit, _ := payload["limit"].(int)
		filter := mpminternal.ConfidenceChangesFilter{
			Since:        time.Now().Add(-24 * time.Hour),
			Limit:        limit,
			ArtifactID:   artifactID,
			ArtifactType: artifactType,
		}
		changes, err := mpminternal.QueryConfidenceChanges(dm, filter)
		if err != nil {
			printError("ops confidence changes: %v", err)
			return 1
		}
		res = map[string]interface{}{"changes": changes, "count": len(changes)}
	case "trend":
		windowDays, _ := payload["window_days"].(int)
		trend, err := mpminternal.QueryConfidenceTrend(dm, artifactID, artifactType, windowDays)
		if err != nil {
			printError("ops confidence trend: %v", err)
			return 1
		}
		res = trend
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	return 0
}
