package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"strconv"

	mpminternal "mpm/internal"
)

// parseEvidenceAddArgs converts a CLI args slice into a map with keys matching
// the EvidenceInput struct fields. Kept separate so it's testable without
// touching the DB.
func parseEvidenceAddArgs(args []string) (map[string]interface{}, error) {
	fs := flag.NewFlagSet("evidence-add", flag.ContinueOnError)
	artifactID := fs.String("artifact", "", "artifact id (required)")
	artifactType := fs.String("artifact-type", "memory", "artifact type (memory/theory/decision/lesson)")
	evType := fs.String("type", "", "evidence type (required)")
	source := fs.String("source", "", "source group (required)")
	strength := fs.String("strength", "", "strength in [-1, 1]; defaults to type's registry value")
	independence := fs.String("independence", "1.0", "independence factor; defaults to 1.0")
	createdBy := fs.String("by", "", "creator (required)")
	notes := fs.String("notes", "", "optional notes")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *artifactID == "" {
		return nil, fmt.Errorf("--artifact is required")
	}
	if *evType == "" {
		return nil, fmt.Errorf("--type is required")
	}
	if !mpminternal.IsValidEvidenceType(*evType) {
		return nil, fmt.Errorf("invalid evidence type: %q", *evType)
	}
	if *source == "" {
		return nil, fmt.Errorf("--source is required")
	}
	if *createdBy == "" {
		return nil, fmt.Errorf("--by is required")
	}
	var s float64
	if *strength == "" {
		def, _ := mpminternal.DefaultStrength(*evType)
		s = def
	} else {
		parsed, err := strconv.ParseFloat(*strength, 64)
		if err != nil {
			return nil, fmt.Errorf("--strength: %w", err)
		}
		s = parsed
	}
	ind, err := strconv.ParseFloat(*independence, 64)
	if err != nil {
		return nil, fmt.Errorf("--independence: %w", err)
	}
	return map[string]interface{}{
		"artifact_id":         *artifactID,
		"artifact_type":       *artifactType,
		"type":                *evType,
		"source_group":        *source,
		"strength":            s,
		"independence_factor": ind,
		"created_by":          *createdBy,
		"notes":               *notes,
	}, nil
}

// handleEvidenceAdd dispatches `mpm evidence add ...`. Calls internal.AddEvidence
// directly. Task 10 will add a parallel `mpm call add_evidence` tool that goes
// through the call layer.
func handleEvidenceAdd(args []string) int {
	payload, err := parseEvidenceAddArgs(args)
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
	in := mpminternal.EvidenceInput{
		ArtifactID:         payload["artifact_id"].(string),
		ArtifactType:       payload["artifact_type"].(string),
		Type:               payload["type"].(string),
		SourceGroup:        payload["source_group"].(string),
		Strength:           payload["strength"].(float64),
		IndependenceFactor: payload["independence_factor"].(float64),
		CreatedBy:          payload["created_by"].(string),
		Notes:              payload["notes"].(string),
	}
	if err := mpminternal.AddEvidence(dm, in); err != nil {
		printError("add evidence: %v", err)
		return 1
	}
	out, _ := json.Marshal(map[string]interface{}{"success": true, "artifact_id": in.ArtifactID, "type": in.Type})
	respond(string(out), "", 0)
	return 0
}

// handleEvidence dispatches `mpm evidence ...` to subcommands. Task 8 will
// add the `list` subcommand.
func handleEvidence(args []string) int {
	if len(args) < 1 {
		printError("usage: mpm evidence <add|list> ...")
		return 1
	}
	switch args[0] {
	case "add":
		return handleEvidenceAdd(args[1:])
	default:
		printError("unknown evidence subcommand: %s", args[0])
		return 1
	}
}
