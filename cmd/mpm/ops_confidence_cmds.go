package main

import (
	"encoding/json"
	"flag"
	"fmt"

	mpminternal "mpm/internal"
)

func parseOpsConfidenceArgs(args []string) (map[string]interface{}, string, error) {
	if len(args) == 0 {
		return nil, "", fmt.Errorf("subcommand required: show|recompute")
	}
	sub := args[0]
	switch sub {
	case "show", "recompute":
	default:
		return nil, "", fmt.Errorf("unknown subcommand: %q (want show|recompute)", sub)
	}
	fs := flag.NewFlagSet("ops-confidence-"+sub, flag.ContinueOnError)
	artifactID := fs.String("artifact", "", "artifact id (required)")
	artifactType := fs.String("artifact-type", "memory", "artifact type")
	limit := fs.Int("limit", 10, "history rows to show (show only)")
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
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	return 0
}
